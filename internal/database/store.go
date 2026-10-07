// Package database contains the optional PostgreSQL integration for the
// gateway. The JSON configuration remains the fallback when no database DSN
// is configured.
package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Wjg-Ares/ai-gateway/internal/codexauth"
	"github.com/Wjg-Ares/ai-gateway/internal/provider"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"

	"github.com/Wjg-Ares/ai-gateway/internal/config"
)

var validSchemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Store owns a small PostgreSQL connection pool. The pool is intentionally
// conservative because the supported ECS profile has less than 1 GB RAM.
type Store struct {
	db               *sql.DB
	schema           string
	encryptionSecret string
	oauthRefreshMu   sync.Mutex
}

func Open(ctx context.Context, dsn, schema string) (*Store, error) {
	if !validSchemaName.MatchString(schema) {
		return nil, fmt.Errorf("invalid database schema %q", schema)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{db: db, schema: schema}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) SetEncryptionSecret(secret string) {
	s.encryptionSecret = secret
}

// LoadModels returns database-defined public model aliases. The caller merges
// these with the JSON routes, so a database row overrides a same-named JSON
// route while JSON remains a safe fallback for models not migrated yet.
func (s *Store) LoadModels(ctx context.Context) (map[string]config.ModelRoute, error) {
	query := fmt.Sprintf(`
		SELECT name, provider, upstream_model
		FROM %s.models
		WHERE enabled = TRUE
		ORDER BY id`, quoteSchema(s.schema))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query models: %w", err)
	}
	defer rows.Close()

	models := make(map[string]config.ModelRoute)
	for rows.Next() {
		var name, providerName, upstreamModel string
		if err := rows.Scan(&name, &providerName, &upstreamModel); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		models[name] = config.ModelRoute{Provider: providerName, UpstreamModel: upstreamModel}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate models: %w", err)
	}
	return models, nil
}

// Validate implements the gateway API-key validator interface. Keys are stored
// as SHA-256 hex digests in gateway_api_keys.key_hash; the raw key never enters
// the database query or logs.
func (s *Store) Validate(ctx context.Context, provided string) (bool, error) {
	digest := sha256.Sum256([]byte(provided))
	hash := hex.EncodeToString(digest[:])
	query := fmt.Sprintf(`
		SELECT EXISTS (
			SELECT 1
			FROM %s.gateway_api_keys
			WHERE key_hash = $1
			  AND status = 'active'
			  AND (expires_at IS NULL OR expires_at > now())
		)`, quoteSchema(s.schema))
	var valid bool
	if err := s.db.QueryRowContext(ctx, query, hash).Scan(&valid); err != nil {
		return false, fmt.Errorf("validate gateway API key: %w", err)
	}
	return valid, nil
}

// GatewayUserID returns the owner of a database-issued gateway key. Static
// environment keys intentionally have no user identity and return found=false.
func (s *Store) GatewayUserID(ctx context.Context, provided string) (int64, bool, error) {
	digest := sha256.Sum256([]byte(provided))
	hash := hex.EncodeToString(digest[:])
	query := fmt.Sprintf(`
		SELECT user_id
		FROM %s.gateway_api_keys
		WHERE key_hash = $1
		  AND status = 'active'
		  AND (expires_at IS NULL OR expires_at > now())`, quoteSchema(s.schema))
	var userID int64
	err := s.db.QueryRowContext(ctx, query, hash).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup gateway key owner: %w", err)
	}
	return userID, true, nil
}

type UserRecord struct {
	ID               int64  `json:"id"`
	Username         string `json:"username"`
	DisplayName      string `json:"display_name"`
	Role             string `json:"role"`
	Status           string `json:"status"`
	SharedModelKeyID *int64 `json:"shared_model_key_id,omitempty"`
}

var ErrInvalidCredentials = fmt.Errorf("invalid username or password")
var ErrProtectedAdmin = errors.New("administrator accounts cannot be changed or deleted here")

type ModelRecord struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
	Enabled       bool   `json:"enabled"`
}

type ModelKeyRecord struct {
	ID             int64  `json:"id"`
	ModelID        int64  `json:"model_id"`
	AccountName    string `json:"account_name"`
	AuthType       string `json:"auth_type"`
	Fingerprint    string `json:"api_key_fingerprint"`
	Status         string `json:"status"`
	Priority       int    `json:"priority"`
	MaxConcurrency int    `json:"max_concurrency"`
}

type GatewayKeyRecord struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	Status    string `json:"status"`
}

type SharedModelKeyRecord struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	ModelKeyID  int64  `json:"model_key_id"`
	AccountName string `json:"account_name"`
	IsDefault   bool   `json:"is_default"`
}

func (s *Store) ListUsers(ctx context.Context) ([]UserRecord, error) {
	query := fmt.Sprintf(`SELECT id, username, COALESCE(display_name, ''), role, status, shared_model_key_id FROM %s.users ORDER BY id`, quoteSchema(s.schema))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	result := make([]UserRecord, 0)
	for rows.Next() {
		var item UserRecord
		if err := rows.Scan(&item.ID, &item.Username, &item.DisplayName, &item.Role, &item.Status, &item.SharedModelKeyID); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CreateUser(ctx context.Context, username, displayName, role, password string) (UserRecord, error) {
	username, role = strings.TrimSpace(username), strings.TrimSpace(role)
	if username == "" {
		return UserRecord{}, fmt.Errorf("username is required")
	}
	if strings.TrimSpace(password) == "" {
		return UserRecord{}, fmt.Errorf("password is required")
	}
	if role == "" {
		role = "user"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return UserRecord{}, fmt.Errorf("hash user password: %w", err)
	}
	query := fmt.Sprintf(`INSERT INTO %s.users (username, display_name, password_hash, role) VALUES ($1, NULLIF($2, ''), $3, $4) RETURNING id, username, COALESCE(display_name, ''), role, status, shared_model_key_id`, quoteSchema(s.schema))
	var item UserRecord
	if err := s.db.QueryRowContext(ctx, query, username, strings.TrimSpace(displayName), string(hash), role).Scan(&item.ID, &item.Username, &item.DisplayName, &item.Role, &item.Status, &item.SharedModelKeyID); err != nil {
		return UserRecord{}, fmt.Errorf("create user: %w", err)
	}
	return item, nil
}

// EnsureAdmin creates or updates the built-in administrator account. The
// password is supplied through the service environment and stored only as a
// bcrypt hash.
func (s *Store) EnsureAdmin(ctx context.Context, password string) error {
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("admin password is not configured")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash admin password: %w", err)
	}
	query := fmt.Sprintf(`INSERT INTO %s.users (username, display_name, password_hash, role, status) VALUES ('admin', 'System Administrator', $1, 'admin', 'active') ON CONFLICT (username) DO UPDATE SET password_hash = EXCLUDED.password_hash, role = 'admin', status = 'active'`, quoteSchema(s.schema))
	if _, err := s.db.ExecContext(ctx, query, string(hash)); err != nil {
		return fmt.Errorf("ensure admin user: %w", err)
	}
	return nil
}

func (s *Store) AuthenticateUser(ctx context.Context, username, password string) (UserRecord, error) {
	query := fmt.Sprintf(`SELECT id, username, COALESCE(display_name, ''), role, status, shared_model_key_id, COALESCE(password_hash, '') FROM %s.users WHERE username = $1`, quoteSchema(s.schema))
	var item UserRecord
	var hash string
	if err := s.db.QueryRowContext(ctx, query, strings.TrimSpace(username)).Scan(&item.ID, &item.Username, &item.DisplayName, &item.Role, &item.Status, &item.SharedModelKeyID, &hash); err != nil {
		if err == sql.ErrNoRows {
			return UserRecord{}, ErrInvalidCredentials
		}
		return UserRecord{}, fmt.Errorf("find user: %w", err)
	}
	if item.Status != "active" || hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return UserRecord{}, ErrInvalidCredentials
	}
	return item, nil
}

func (s *Store) SetUserPassword(ctx context.Context, userID int64, password string) error {
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("password is required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash user password: %w", err)
	}
	query := fmt.Sprintf(`UPDATE %s.users SET password_hash = $2 WHERE id = $1 AND username <> 'admin' AND role <> 'admin'`, quoteSchema(s.schema))
	result, err := s.db.ExecContext(ctx, query, userID, string(hash))
	if err != nil {
		return fmt.Errorf("set user password: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set user password: %w", err)
	}
	if count != 1 {
		var role string
		check := fmt.Sprintf(`SELECT role FROM %s.users WHERE id = $1`, quoteSchema(s.schema))
		if err := s.db.QueryRowContext(ctx, check, userID).Scan(&role); err == nil && role == "admin" {
			return ErrProtectedAdmin
		}
		return fmt.Errorf("user %d was not found", userID)
	}
	return nil
}

func (s *Store) DeleteUser(ctx context.Context, userID int64) error {
	query := fmt.Sprintf(`DELETE FROM %s.users WHERE id = $1 AND username <> 'admin' AND role <> 'admin'`, quoteSchema(s.schema))
	result, err := s.db.ExecContext(ctx, query, userID)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		var role string
		check := fmt.Sprintf(`SELECT role FROM %s.users WHERE id = $1`, quoteSchema(s.schema))
		if err := s.db.QueryRowContext(ctx, check, userID).Scan(&role); err == nil && role == "admin" {
			return ErrProtectedAdmin
		}
		return fmt.Errorf("user not found or administrator cannot be deleted")
	}
	return nil
}

func (s *Store) DeleteModel(ctx context.Context, modelID int64) error {
	query := fmt.Sprintf(`DELETE FROM %s.models WHERE id = $1`, quoteSchema(s.schema))
	result, err := s.db.ExecContext(ctx, query, modelID)
	if err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("model not found")
	}
	return nil
}

func (s *Store) DeleteModelKey(ctx context.Context, modelKeyID int64, userID int64, admin bool) error {
	query := fmt.Sprintf(`DELETE FROM %s.model_keys WHERE id = $1`, quoteSchema(s.schema))
	args := []any{modelKeyID}
	if !admin {
		query = fmt.Sprintf(`DELETE FROM %s.model_keys WHERE id = $1 AND created_by_user_id = $2`, quoteSchema(s.schema))
		args = append(args, userID)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("delete model key: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("model key not found or not owned by current user")
	}
	return nil
}

func (s *Store) ListModelsAdmin(ctx context.Context) ([]ModelRecord, error) {
	query := fmt.Sprintf(`SELECT id, name, provider, upstream_model, enabled FROM %s.models ORDER BY id`, quoteSchema(s.schema))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer rows.Close()
	result := make([]ModelRecord, 0)
	for rows.Next() {
		var item ModelRecord
		if err := rows.Scan(&item.ID, &item.Name, &item.Provider, &item.UpstreamModel, &item.Enabled); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CreateModel(ctx context.Context, name, providerName, upstreamModel string) (ModelRecord, error) {
	query := fmt.Sprintf(`INSERT INTO %s.models (name, provider, upstream_model) VALUES ($1, $2, $3) RETURNING id, name, provider, upstream_model, enabled`, quoteSchema(s.schema))
	var item ModelRecord
	if err := s.db.QueryRowContext(ctx, query, strings.TrimSpace(name), strings.TrimSpace(providerName), strings.TrimSpace(upstreamModel)).Scan(&item.ID, &item.Name, &item.Provider, &item.UpstreamModel, &item.Enabled); err != nil {
		return ModelRecord{}, fmt.Errorf("create model: %w", err)
	}
	return item, nil
}

func (s *Store) ListModelKeys(ctx context.Context, modelID int64) ([]ModelKeyRecord, error) {
	return s.ListModelKeysForModels(ctx, []int64{modelID}, 0, true)
}

func (s *Store) ListModelKeysForUser(ctx context.Context, modelID, userID int64) ([]ModelKeyRecord, error) {
	return s.ListModelKeysForModels(ctx, []int64{modelID}, userID, false)
}

// ListModelKeysForModels returns each key once when it is associated with any
// of the requested models. userID=0 with admin=true lists all owners.
func (s *Store) ListModelKeysForModels(ctx context.Context, modelIDs []int64, userID int64, admin bool) ([]ModelKeyRecord, error) {
	modelIDs = normalizeIDs(modelIDs)
	if len(modelIDs) == 0 {
		return []ModelKeyRecord{}, nil
	}
	placeholders := make([]string, len(modelIDs))
	args := make([]any, len(modelIDs))
	for i, id := range modelIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT k.id, k.model_id, k.account_name, k.auth_type, k.api_key_fingerprint, k.status, k.priority, k.max_concurrency
		FROM %s.model_keys k
		WHERE (k.model_id IN (%s) OR EXISTS (
			SELECT 1 FROM %s.model_key_models km
			WHERE km.model_key_id = k.id AND km.model_id IN (%s)
		))`, quoteSchema(s.schema), strings.Join(placeholders, ","), quoteSchema(s.schema), strings.Join(placeholders, ","))
	if !admin {
		query += fmt.Sprintf(" AND k.created_by_user_id = $%d", len(args)+1)
		args = append(args, userID)
	}
	query += " ORDER BY k.priority DESC, k.id"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list model keys: %w", err)
	}
	defer rows.Close()
	result := make([]ModelKeyRecord, 0)
	for rows.Next() {
		var item ModelKeyRecord
		var modelID sql.NullInt64
		if err := rows.Scan(&item.ID, &modelID, &item.AccountName, &item.AuthType, &item.Fingerprint, &item.Status, &item.Priority, &item.MaxConcurrency); err != nil {
			return nil, fmt.Errorf("scan model key: %w", err)
		}
		if modelID.Valid {
			item.ModelID = modelID.Int64
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CreateCodexOAuthKey(ctx context.Context, modelIDs []int64, ownerUserID int64, accountName string, bundle codexauth.TokenBundle, encryptionSecret string) (ModelKeyRecord, error) {
	modelIDs = normalizeIDs(modelIDs)
	if len(modelIDs) == 0 {
		return ModelKeyRecord{}, fmt.Errorf("at least one model is required")
	}
	if strings.TrimSpace(encryptionSecret) == "" {
		return ModelKeyRecord{}, fmt.Errorf("AI_GATEWAY_KEY_ENCRYPTION_SECRET is not configured")
	}
	payload, err := json.Marshal(bundle)
	if err != nil {
		return ModelKeyRecord{}, fmt.Errorf("encode OAuth token bundle: %w", err)
	}
	ciphertext, err := encrypt(string(payload), encryptionSecret)
	if err != nil {
		return ModelKeyRecord{}, err
	}
	fingerprint := sha256.Sum256([]byte(bundle.RefreshToken))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelKeyRecord{}, fmt.Errorf("begin OAuth key: %w", err)
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`INSERT INTO %s.model_keys (model_id, created_by_user_id, account_name, auth_type, api_key_ciphertext, api_key_fingerprint) VALUES ($1, $2, $3, 'codex_oauth', $4, $5)
		ON CONFLICT (api_key_fingerprint) DO UPDATE SET
			model_id = EXCLUDED.model_id,
			account_name = EXCLUDED.account_name,
			auth_type = 'codex_oauth',
			api_key_ciphertext = EXCLUDED.api_key_ciphertext,
			status = 'active',
			updated_at = now()
		WHERE model_keys.created_by_user_id = EXCLUDED.created_by_user_id
		RETURNING id, model_id, account_name, auth_type, api_key_fingerprint, status, priority, max_concurrency`, quoteSchema(s.schema))
	var item ModelKeyRecord
	if err := tx.QueryRowContext(ctx, query, modelIDs[0], ownerUserID, strings.TrimSpace(accountName), ciphertext, hex.EncodeToString(fingerprint[:])).Scan(&item.ID, &item.ModelID, &item.AccountName, &item.AuthType, &item.Fingerprint, &item.Status, &item.Priority, &item.MaxConcurrency); err != nil {
		return ModelKeyRecord{}, fmt.Errorf("create Codex OAuth key: %w", err)
	}
	if err := insertModelKeyModels(ctx, tx, s.schema, item.ID, modelIDs); err != nil {
		return ModelKeyRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return ModelKeyRecord{}, fmt.Errorf("commit Codex OAuth key: %w", err)
	}
	return item, nil
}

// CreateModelKey encrypts the upstream secret before storing it. The raw key
// is never returned by the admin API and is not written to PostgreSQL.
func (s *Store) CreateModelKey(ctx context.Context, modelIDs []int64, ownerUserID int64, accountName, rawKey string, priority, maxConcurrency int, encryptionSecret string) (ModelKeyRecord, error) {
	modelIDs = normalizeIDs(modelIDs)
	if len(modelIDs) == 0 {
		return ModelKeyRecord{}, fmt.Errorf("at least one model is required")
	}
	if strings.TrimSpace(rawKey) == "" {
		return ModelKeyRecord{}, fmt.Errorf("provider key is required")
	}
	if strings.TrimSpace(encryptionSecret) == "" {
		return ModelKeyRecord{}, fmt.Errorf("AI_GATEWAY_KEY_ENCRYPTION_SECRET is not configured")
	}
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	ciphertext, err := encrypt(rawKey, encryptionSecret)
	if err != nil {
		return ModelKeyRecord{}, err
	}
	fingerprint := sha256.Sum256([]byte(rawKey))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelKeyRecord{}, fmt.Errorf("begin model key: %w", err)
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`INSERT INTO %s.model_keys (model_id, created_by_user_id, account_name, api_key_ciphertext, api_key_fingerprint, priority, max_concurrency) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, model_id, account_name, auth_type, api_key_fingerprint, status, priority, max_concurrency`, quoteSchema(s.schema))
	var item ModelKeyRecord
	if err := tx.QueryRowContext(ctx, query, modelIDs[0], ownerUserID, strings.TrimSpace(accountName), ciphertext, hex.EncodeToString(fingerprint[:]), priority, maxConcurrency).Scan(&item.ID, &item.ModelID, &item.AccountName, &item.AuthType, &item.Fingerprint, &item.Status, &item.Priority, &item.MaxConcurrency); err != nil {
		return ModelKeyRecord{}, fmt.Errorf("create model key: %w", err)
	}
	if err := insertModelKeyModels(ctx, tx, s.schema, item.ID, modelIDs); err != nil {
		return ModelKeyRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return ModelKeyRecord{}, fmt.Errorf("commit model key: %w", err)
	}
	return item, nil
}

func insertModelKeyModels(ctx context.Context, tx *sql.Tx, schema string, modelKeyID int64, modelIDs []int64) error {
	query := fmt.Sprintf(`INSERT INTO %s.model_key_models (model_key_id, model_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, quoteSchema(schema))
	for _, modelID := range modelIDs {
		if _, err := tx.ExecContext(ctx, query, modelKeyID, modelID); err != nil {
			return fmt.Errorf("associate model key with model %d: %w", modelID, err)
		}
	}
	return nil
}

func normalizeIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				result = append(result, id)
			}
		}
	}
	return result
}

func (s *Store) AssignSharedModelKey(ctx context.Context, userID, modelKeyID int64, makeDefault bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin assignment: %w", err)
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`INSERT INTO %s.user_shared_model_keys (user_id, model_key_id, is_default) VALUES ($1, $2, $3) ON CONFLICT (user_id, model_key_id) DO UPDATE SET is_default = EXCLUDED.is_default`, quoteSchema(s.schema))
	if _, err := tx.ExecContext(ctx, query, userID, modelKeyID, makeDefault); err != nil {
		return fmt.Errorf("assign model key: %w", err)
	}
	if makeDefault {
		query = fmt.Sprintf(`UPDATE %s.users SET shared_model_key_id = $2 WHERE id = $1`, quoteSchema(s.schema))
		if _, err := tx.ExecContext(ctx, query, userID, modelKeyID); err != nil {
			return fmt.Errorf("set default shared key: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit assignment: %w", err)
	}
	return nil
}

func (s *Store) ListSharedModelKeys(ctx context.Context) ([]SharedModelKeyRecord, error) {
	query := fmt.Sprintf(`
		SELECT a.user_id, u.username, a.model_key_id, k.account_name, a.is_default
		FROM %s.user_shared_model_keys a
		JOIN %s.users u ON u.id = a.user_id
		JOIN %s.model_keys k ON k.id = a.model_key_id
		ORDER BY a.user_id, a.model_key_id`, quoteSchema(s.schema), quoteSchema(s.schema), quoteSchema(s.schema))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list shared model keys: %w", err)
	}
	defer rows.Close()
	result := make([]SharedModelKeyRecord, 0)
	for rows.Next() {
		var item SharedModelKeyRecord
		if err := rows.Scan(&item.UserID, &item.Username, &item.ModelKeyID, &item.AccountName, &item.IsDefault); err != nil {
			return nil, fmt.Errorf("scan shared model key: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) DeleteSharedModelKey(ctx context.Context, userID, modelKeyID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin assignment deletion: %w", err)
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`DELETE FROM %s.user_shared_model_keys WHERE user_id = $1 AND model_key_id = $2`, quoteSchema(s.schema))
	result, err := tx.ExecContext(ctx, query, userID, modelKeyID)
	if err != nil {
		return fmt.Errorf("delete shared model key: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("shared model key assignment not found")
	}
	query = fmt.Sprintf(`UPDATE %s.users SET shared_model_key_id = NULL WHERE id = $1 AND shared_model_key_id = $2`, quoteSchema(s.schema))
	if _, err := tx.ExecContext(ctx, query, userID, modelKeyID); err != nil {
		return fmt.Errorf("clear default shared model key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit assignment deletion: %w", err)
	}
	return nil
}

// ResolveModelCredential selects an active API key owned by the user or
// explicitly shared with the user for the requested public model alias.
func (s *Store) ResolveModelCredential(ctx context.Context, userID int64, publicModel string) (provider.Credential, bool, error) {
	if userID <= 0 || strings.TrimSpace(s.encryptionSecret) == "" {
		return provider.Credential{}, false, nil
	}
	query := fmt.Sprintf(`
		SELECT k.id, k.auth_type, COALESCE(k.api_key_ciphertext, ''), k.max_concurrency
		FROM %s.model_keys k
		JOIN %s.models m ON m.name = $2
		WHERE k.status = 'active'
		  AND k.auth_type IN ('api_key', 'codex_oauth')
		  AND (k.model_id = m.id OR EXISTS (
			SELECT 1 FROM %s.model_key_models km
			WHERE km.model_key_id = k.id AND km.model_id = m.id
		  ))
		  AND (k.created_by_user_id = $1 OR EXISTS (
			SELECT 1 FROM %s.user_shared_model_keys a
			WHERE a.user_id = $1 AND a.model_key_id = k.id
		  ))
		ORDER BY k.priority DESC, k.id
		LIMIT 1`, quoteSchema(s.schema), quoteSchema(s.schema), quoteSchema(s.schema), quoteSchema(s.schema))
	var keyID int64
	var authType string
	var ciphertext string
	var maxConcurrency int
	if err := s.db.QueryRowContext(ctx, query, userID, strings.TrimSpace(publicModel)).Scan(&keyID, &authType, &ciphertext, &maxConcurrency); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return provider.Credential{}, false, nil
		}
		return provider.Credential{}, false, fmt.Errorf("select model credential: %w", err)
	}
	if authType == "api_key" {
		plaintext, err := decrypt(ciphertext, s.encryptionSecret)
		if err != nil {
			return provider.Credential{}, false, fmt.Errorf("decrypt model credential: %w", err)
		}
		return provider.Credential{APIKey: plaintext, AuthType: authType, KeyID: keyID, MaxConcurrency: maxConcurrency}, true, nil
	}
	if authType != "codex_oauth" {
		return provider.Credential{}, false, nil
	}
	// Refresh-token rotation can make a second concurrent refresh invalidate the
	// first result. Re-read the row after taking the process-wide lock so every
	// waiter sees the bundle written by the preceding refresh.
	s.oauthRefreshMu.Lock()
	defer s.oauthRefreshMu.Unlock()
	currentQuery := fmt.Sprintf(`SELECT auth_type, COALESCE(api_key_ciphertext, '') FROM %s.model_keys WHERE id = $1 AND status = 'active'`, quoteSchema(s.schema))
	if err := s.db.QueryRowContext(ctx, currentQuery, keyID).Scan(&authType, &ciphertext); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return provider.Credential{}, false, nil
		}
		return provider.Credential{}, false, fmt.Errorf("reload model credential: %w", err)
	}
	if authType != "codex_oauth" {
		return provider.Credential{}, false, nil
	}
	plaintext, err := decrypt(ciphertext, s.encryptionSecret)
	if err != nil {
		return provider.Credential{}, false, fmt.Errorf("decrypt model credential: %w", err)
	}
	var bundle codexauth.TokenBundle
	if err := json.Unmarshal([]byte(plaintext), &bundle); err != nil {
		return provider.Credential{}, false, fmt.Errorf("decode OAuth token bundle: %w", err)
	}
	if bundle.AccessToken == "" || bundle.RefreshToken == "" {
		return provider.Credential{}, false, fmt.Errorf("OAuth token bundle is incomplete")
	}
	if !codexauth.HasDirectResponsesScope(bundle.Scope) {
		return provider.Credential{}, false, fmt.Errorf("OAuth token lacks chatgpt.tokens.use.direct; sign in again from the ChatGPT/Codex button")
	}
	if time.Until(bundle.ExpiresAt) <= 60*time.Second {
		refreshed, refreshErr := codexauth.RefreshWithClient(ctx, bundle.RefreshToken, bundle.ClientID)
		if refreshErr != nil {
			return provider.Credential{}, false, refreshErr
		}
		if refreshed.Scope == "" {
			refreshed.Scope = bundle.Scope
		}
		if refreshed.IDToken == "" {
			refreshed.IDToken = bundle.IDToken
		}
		if refreshed.Subject == "" {
			refreshed.Subject = bundle.Subject
		}
		if refreshed.Email == "" {
			refreshed.Email = bundle.Email
		}
		if refreshed.HostID == "" {
			refreshed.HostID = bundle.HostID
		}
		encoded, encodeErr := json.Marshal(refreshed)
		if encodeErr != nil {
			return provider.Credential{}, false, fmt.Errorf("encode refreshed OAuth token: %w", encodeErr)
		}
		updated, encryptErr := encrypt(string(encoded), s.encryptionSecret)
		if encryptErr != nil {
			return provider.Credential{}, false, encryptErr
		}
		if err := s.updateModelKeyCiphertext(ctx, keyID, updated); err != nil {
			return provider.Credential{}, false, err
		}
		bundle = refreshed
	}
	return provider.Credential{APIKey: bundle.AccessToken, AuthType: authType, KeyID: keyID, MaxConcurrency: maxConcurrency}, true, nil
}

func (s *Store) updateModelKeyCiphertext(ctx context.Context, keyID int64, ciphertext string) error {
	query := fmt.Sprintf(`UPDATE %s.model_keys SET api_key_ciphertext = $2, updated_at = now() WHERE id = $1`, quoteSchema(s.schema))
	if _, err := s.db.ExecContext(ctx, query, keyID, ciphertext); err != nil {
		return fmt.Errorf("save refreshed OAuth token: %w", err)
	}
	return nil
}

func (s *Store) CreateGatewayKey(ctx context.Context, userID int64, name string) (string, error) {
	raw, err := randomSecret(32)
	if err != nil {
		return "", fmt.Errorf("generate gateway key: %w", err)
	}
	digest := sha256.Sum256([]byte(raw))
	prefix := raw
	if len(prefix) > 10 {
		prefix = prefix[:10]
	}
	query := fmt.Sprintf(`INSERT INTO %s.gateway_api_keys (user_id, name, key_prefix, key_hash) VALUES ($1, $2, $3, $4)`, quoteSchema(s.schema))
	if _, err := s.db.ExecContext(ctx, query, userID, strings.TrimSpace(name), prefix, hex.EncodeToString(digest[:])); err != nil {
		return "", fmt.Errorf("create gateway key: %w", err)
	}
	return raw, nil
}

func (s *Store) ListGatewayKeys(ctx context.Context) ([]GatewayKeyRecord, error) {
	query := fmt.Sprintf(`SELECT k.id, k.user_id, u.username, k.name, k.key_prefix, k.status FROM %s.gateway_api_keys k JOIN %s.users u ON u.id = k.user_id ORDER BY k.id DESC`, quoteSchema(s.schema), quoteSchema(s.schema))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list gateway keys: %w", err)
	}
	defer rows.Close()
	result := make([]GatewayKeyRecord, 0)
	for rows.Next() {
		var item GatewayKeyRecord
		if err := rows.Scan(&item.ID, &item.UserID, &item.Username, &item.Name, &item.KeyPrefix, &item.Status); err != nil {
			return nil, fmt.Errorf("scan gateway key: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) DeleteGatewayKey(ctx context.Context, keyID int64) error {
	query := fmt.Sprintf(`DELETE FROM %s.gateway_api_keys WHERE id = $1`, quoteSchema(s.schema))
	result, err := s.db.ExecContext(ctx, query, keyID)
	if err != nil {
		return fmt.Errorf("delete gateway key: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("gateway key not found")
	}
	return nil
}

func encrypt(plaintext, secret string) (string, error) {
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("create encryption cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create encryption mode: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate encryption nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return "v1:" + base64.RawStdEncoding.EncodeToString(sealed), nil
}

func decrypt(ciphertext, secret string) (string, error) {
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("create decryption cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create decryption mode: %w", err)
	}
	if !strings.HasPrefix(ciphertext, "v1:") {
		return "", fmt.Errorf("unsupported encrypted credential format")
	}
	sealed, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(ciphertext, "v1:"))
	if err != nil {
		return "", fmt.Errorf("decode encrypted credential: %w", err)
	}
	if len(sealed) < gcm.NonceSize() {
		return "", fmt.Errorf("encrypted credential is truncated")
	}
	plaintext, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("open encrypted credential: %w", err)
	}
	return string(plaintext), nil
}

func randomSecret(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func quoteSchema(schema string) string {
	// Open rejects anything outside the conservative identifier grammar above.
	return `"` + schema + `"`
}
