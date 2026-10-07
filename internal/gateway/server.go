package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Wjg-Ares/ai-gateway/internal/router"
)

type Server struct {
	http *http.Server
}

func NewServer(address, gatewayAPIKey string, requestRouter router.Router) *Server {
	return NewServerWithAuthenticator(address, NewStaticAuthenticator(gatewayAPIKey), requestRouter)
}

// APIKeyAuthenticator validates a gateway client key. Implementations may use
// a static environment value, PostgreSQL, or both during migration.
type APIKeyAuthenticator interface {
	Validate(context.Context, string) (bool, error)
}

type gatewayUserLookup interface {
	GatewayUserID(context.Context, string) (int64, bool, error)
}

type staticAuthenticator struct {
	expected string
}

func NewStaticAuthenticator(expected string) APIKeyAuthenticator {
	return staticAuthenticator{expected: expected}
}

func (a staticAuthenticator) Validate(_ context.Context, provided string) (bool, error) {
	if a.expected == "" || len(provided) != len(a.expected) {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(a.expected)) == 1, nil
}

// CompositeAuthenticator keeps the existing environment key working while
// database-backed gateway keys are being introduced.
type CompositeAuthenticator struct {
	static APIKeyAuthenticator
	db     APIKeyAuthenticator
}

func NewCompositeAuthenticator(static, db APIKeyAuthenticator) APIKeyAuthenticator {
	return CompositeAuthenticator{static: static, db: db}
}

func (a CompositeAuthenticator) Validate(ctx context.Context, provided string) (bool, error) {
	if a.static != nil {
		valid, err := a.static.Validate(ctx, provided)
		if err != nil || valid {
			return valid, err
		}
	}
	if a.db == nil {
		return false, nil
	}
	return a.db.Validate(ctx, provided)
}

func (a CompositeAuthenticator) GatewayUserID(ctx context.Context, provided string) (int64, bool, error) {
	if a.static != nil {
		valid, err := a.static.Validate(ctx, provided)
		if err != nil || valid {
			return 0, false, err
		}
	}
	lookup, ok := a.db.(gatewayUserLookup)
	if !ok {
		return 0, false, nil
	}
	return lookup.GatewayUserID(ctx, provided)
}

func NewServerWithAuthenticator(address string, authenticator APIKeyAuthenticator, requestRouter router.Router) *Server {
	return NewServerWithAdmin(address, authenticator, requestRouter, nil)
}

// NewServerWithAdmin mounts an optional administrator UI/API below /admin/.
// The admin handler owns its own authentication and is kept separate from
// client gateway-key authentication.
func NewServerWithAdmin(address string, authenticator APIKeyAuthenticator, requestRouter router.Router, adminHandler http.Handler) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler)
	mux.Handle("GET /v1/models", authenticate(authenticator, modelsHandler(requestRouter)))
	mux.Handle("POST /v1/messages", authenticate(authenticator, http.HandlerFunc(anthropicMessagesHandler(requestRouter))))
	mux.Handle("POST /v1/messages/count_tokens", authenticate(authenticator, http.HandlerFunc(anthropicMessagesHandler(requestRouter))))
	if adminHandler != nil {
		mux.Handle("/admin/", adminHandler)
	}

	return &Server{http: &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}}
}

func (s *Server) ListenAndServe() error { return s.http.ListenAndServe() }

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

type modelListResponse struct {
	Object string            `json:"object"`
	Data   []modelListRecord `json:"data"`
}

type modelListRecord struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func modelsHandler(requestRouter router.Router) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lister, ok := requestRouter.(router.ModelLister)
		if !ok {
			writeOpenAIError(w, http.StatusNotImplemented, "model listing is not implemented")
			return
		}
		userID, _ := router.RequestUserID(r.Context())
		models, err := lister.ListModels(r.Context(), userID)
		if err != nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "model listing is unavailable")
			return
		}
		items := make([]modelListRecord, 0, len(models))
		for _, model := range models {
			items = append(items, modelListRecord{ID: model, Object: "model", OwnedBy: "ai-gateway"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(modelListResponse{Object: "list", Data: items})
	})
}

func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": message, "type": "server_error"},
	})
}

func authenticate(authenticator APIKeyAuthenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("x-api-key")
		if provided == "" {
			scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
			if found && strings.EqualFold(scheme, "Bearer") {
				provided = strings.TrimSpace(token)
			}
		}
		valid, err := authenticator.Validate(r.Context(), provided)
		if err != nil {
			writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "gateway authentication backend is unavailable")
			return
		}
		if !valid {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid or missing gateway API key")
			return
		}
		if lookup, ok := authenticator.(gatewayUserLookup); ok {
			userID, found, lookupErr := lookup.GatewayUserID(r.Context(), provided)
			if lookupErr != nil {
				writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "gateway user lookup is unavailable")
				return
			}
			if found {
				r = r.WithContext(router.WithRequestUserID(r.Context(), userID))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeAnthropicError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": message},
	})
}
