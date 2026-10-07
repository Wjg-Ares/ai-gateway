package codexauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	legacyClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	authorizeURL      = "https://auth.openai.com/api/accounts/authorize"
	tokenURL          = "https://auth.openai.com/api/accounts/oauth/token"
	legacyUserCodeURL = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	legacyTokenURL    = "https://auth.openai.com/api/accounts/deviceauth/token"
	legacyOAuthURL    = "https://auth.openai.com/oauth/token"
	legacyVerifyURL   = "https://auth.openai.com/codex/device"
	legacyRedirectURI = "https://auth.openai.com/deviceauth/callback"
	resource          = "https://api.openai.com/v1"
	scope             = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	issuer            = "https://auth.openai.com"
	jwksURL           = "https://auth.openai.com/.well-known/jwks.json"
)

type DeviceCode struct {
	DeviceAuthID    string `json:"device_auth_id"`
	UserCode        string `json:"user_code"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
	VerificationURI string `json:"verification_uri"`
}

func (d *DeviceCode) UnmarshalJSON(data []byte) error {
	var raw struct {
		DeviceAuthID    string          `json:"device_auth_id"`
		UserCode        string          `json:"user_code"`
		Interval        json.RawMessage `json:"interval"`
		ExpiresIn       json.RawMessage `json:"expires_in"`
		VerificationURI string          `json:"verification_uri"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	interval, err := flexibleInt(raw.Interval)
	if err != nil {
		return fmt.Errorf("decode interval: %w", err)
	}
	expires, err := flexibleInt(raw.ExpiresIn)
	if err != nil {
		return fmt.Errorf("decode expires_in: %w", err)
	}
	*d = DeviceCode{DeviceAuthID: raw.DeviceAuthID, UserCode: raw.UserCode, Interval: interval, ExpiresIn: expires, VerificationURI: raw.VerificationURI}
	return nil
}
func flexibleInt(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

// TokenBundle is encrypted in the database as JSON. The registration fields
// are needed by the current Sign in with ChatGPT flow and remain optional for
// old rows created by the legacy device flow.
type TokenBundle struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	Subject      string    `json:"subject,omitempty"`
	Email        string    `json:"email,omitempty"`
	HostID       string    `json:"ext_agent_host_id,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}
type AuthorizationRequest struct{ URL, State, Verifier, Nonce, ClientID, RedirectURI, HostID string }
type Callback struct{ Code, State, ClientID, Scope, Error, ErrorDescription string }

func StartAuthorization(_ context.Context, redirectURI, hostID string) (AuthorizationRequest, error) {
	if !strings.HasPrefix(redirectURI, "http://127.0.0.1:") || !strings.HasSuffix(redirectURI, "/admin/api/codex/callback") {
		return AuthorizationRequest{}, errors.New("OAuth callback must use http://127.0.0.1:<port>/admin/api/codex/callback")
	}
	state, err := randomValue(32)
	if err != nil {
		return AuthorizationRequest{}, err
	}
	nonce, err := randomValue(32)
	if err != nil {
		return AuthorizationRequest{}, err
	}
	verifier, err := randomValue(48)
	if err != nil {
		return AuthorizationRequest{}, err
	}
	digest := sha256.Sum256([]byte(verifier))
	clientID := "dynamic_agent_client"
	q := url.Values{"client_id": {clientID}, "agent_name_hint": {"AI Gateway"}, "ext_agent_host_id": {hostID}, "response_type": {"code"}, "redirect_uri": {redirectURI}, "scope": {scope}, "resource": {resource}, "state": {state}, "nonce": {nonce}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}}
	return AuthorizationRequest{URL: authorizeURL + "?" + q.Encode(), State: state, Verifier: verifier, Nonce: nonce, ClientID: clientID, RedirectURI: redirectURI, HostID: hostID}, nil
}
func randomValue(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func ExchangeCallback(ctx context.Context, cb Callback, req AuthorizationRequest) (TokenBundle, error) {
	if cb.Error != "" {
		if cb.ErrorDescription != "" {
			return TokenBundle{}, fmt.Errorf("OpenAI OAuth: %s", cb.ErrorDescription)
		}
		return TokenBundle{}, fmt.Errorf("OpenAI OAuth: %s", cb.Error)
	}
	if cb.Code == "" || subtle.ConstantTimeCompare([]byte(cb.State), []byte(req.State)) != 1 {
		return TokenBundle{}, errors.New("invalid or missing OAuth state/code")
	}
	clientID := cb.ClientID
	if clientID == "" {
		clientID = req.ClientID
	}
	if clientID == "dynamic_agent_client" {
		return TokenBundle{}, errors.New("OpenAI did not return an issued client_id")
	}
	resp, err := postForm(ctx, tokenURL, url.Values{"grant_type": {"authorization_code"}, "code": {cb.Code}, "redirect_uri": {req.RedirectURI}, "client_id": {clientID}, "code_verifier": {req.Verifier}, "resource": {resource}})
	if err != nil {
		return TokenBundle{}, fmt.Errorf("exchange OpenAI OAuth code: %w", err)
	}
	var raw tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		resp.Body.Close()
		return TokenBundle{}, fmt.Errorf("decode OpenAI OAuth token: %w", err)
	}
	resp.Body.Close()
	granted := firstNonEmpty(raw.Scope, cb.Scope)
	if raw.AccessToken == "" || raw.RefreshToken == "" {
		return TokenBundle{}, errors.New("OpenAI OAuth response did not include tokens")
	}
	if !hasScope(granted, "chatgpt.tokens.use.direct") {
		return TokenBundle{}, errors.New("ChatGPT plan permission was not granted: missing chatgpt.tokens.use.direct")
	}
	claims, err := validateIDToken(ctx, raw.IDToken, clientID, req.Nonce)
	if err != nil {
		return TokenBundle{}, err
	}
	if raw.ExpiresIn <= 0 {
		raw.ExpiresIn = 3600
	}
	return TokenBundle{AccessToken: raw.AccessToken, RefreshToken: raw.RefreshToken, IDToken: raw.IDToken, ClientID: clientID, Scope: granted, Subject: claims.Subject, Email: claims.Email, HostID: req.HostID, ExpiresAt: time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)}, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func hasScope(scopes, want string) bool {
	for _, s := range strings.Fields(scopes) {
		if s == want {
			return true
		}
	}
	return false
}

// HasDirectResponsesScope reports whether a token exchange granted the scope
// required for ChatGPT plan usage through the public Responses API.
func HasDirectResponsesScope(scopes string) bool {
	return hasScope(scopes, "chatgpt.tokens.use.direct")
}
func postForm(ctx context.Context, endpoint string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "ai-gateway-siwc/1.0")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var b struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&b)
		return nil, fmt.Errorf("OpenAI OAuth returned HTTP %d: %s", resp.StatusCode, firstNonEmpty(b.Description, b.Error))
	}
	return resp, nil
}

type idClaims struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Audience  json.RawMessage `json:"aud"`
	Nonce     string          `json:"nonce"`
	Email     string          `json:"email"`
	ExpiresAt int64           `json:"exp"`
}
type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
}
type jwkSet struct {
	Keys []struct {
		KeyType string `json:"kty"`
		KeyID   string `json:"kid"`
		N       string `json:"n"`
		E       string `json:"e"`
	} `json:"keys"`
}

var jwksCache struct {
	sync.Mutex
	at   time.Time
	keys jwkSet
}

func validateIDToken(ctx context.Context, token, clientID, nonce string) (idClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return idClaims{}, errors.New("OpenAI OAuth response contained an invalid ID token")
	}
	decode := func(s string, v any) error {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			b, err = base64.URLEncoding.DecodeString(s)
		}
		if err != nil {
			return err
		}
		return json.Unmarshal(b, v)
	}
	var h jwtHeader
	var c idClaims
	if err := decode(parts[0], &h); err != nil || h.Algorithm != "RS256" {
		return idClaims{}, errors.New("OpenAI ID token uses an unsupported signature")
	}
	if err := decode(parts[1], &c); err != nil {
		return idClaims{}, errors.New("OpenAI ID token claims are invalid")
	}
	if c.Issuer != issuer || c.Subject == "" || c.Nonce != nonce || c.ExpiresAt <= time.Now().Unix() || !audienceContains(c.Audience, clientID) {
		return idClaims{}, errors.New("OpenAI ID token validation failed")
	}
	set, err := loadJWKS(ctx)
	if err != nil {
		return idClaims{}, err
	}
	var key *rsa.PublicKey
	for _, k := range set.Keys {
		if k.KeyID != h.KeyID || k.KeyType != "RSA" {
			continue
		}
		n, e := base64.RawURLEncoding.DecodeString(k.N)
		if e != nil {
			return idClaims{}, e
		}
		eb, e := base64.RawURLEncoding.DecodeString(k.E)
		if e != nil {
			return idClaims{}, e
		}
		exp := 0
		for _, b := range eb {
			exp = exp*256 + int(b)
		}
		key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
		break
	}
	if key == nil {
		return idClaims{}, errors.New("OpenAI ID token signing key was not found")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return idClaims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return idClaims{}, errors.New("OpenAI ID token signature validation failed")
	}
	return c, nil
}
func audienceContains(raw json.RawMessage, want string) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, v := range many {
			if v == want {
				return true
			}
		}
	}
	return false
}
func loadJWKS(ctx context.Context) (jwkSet, error) {
	jwksCache.Lock()
	defer jwksCache.Unlock()
	if time.Since(jwksCache.at) < time.Hour && len(jwksCache.keys.Keys) > 0 {
		return jwksCache.keys, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return jwkSet{}, err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return jwkSet{}, fmt.Errorf("load OpenAI JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return jwkSet{}, fmt.Errorf("load OpenAI JWKS returned HTTP %d", resp.StatusCode)
	}
	var set jwkSet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return jwkSet{}, err
	}
	jwksCache.keys, jwksCache.at = set, time.Now()
	return set, nil
}

// Legacy functions remain for old callers, but the admin UI now uses the
// authorization-code flow above.
func Start(ctx context.Context) (DeviceCode, error) {
	body, _ := json.Marshal(map[string]string{"client_id": legacyClientID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, legacyUserCodeURL, bytes.NewReader(body))
	if err != nil {
		return DeviceCode{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return DeviceCode{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DeviceCode{}, fmt.Errorf("OpenAI device code returned HTTP %d", resp.StatusCode)
	}
	var result DeviceCode
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return DeviceCode{}, fmt.Errorf("decode device code: %w", err)
	}
	if result.Interval <= 0 {
		result.Interval = 5
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 900
	}
	result.VerificationURI = legacyVerifyURL
	return result, nil
}
func Poll(ctx context.Context, deviceAuthID, userCode string) (TokenBundle, bool, error) {
	body, _ := json.Marshal(map[string]string{"device_auth_id": deviceAuthID, "user_code": userCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, legacyTokenURL, bytes.NewReader(body))
	if err != nil {
		return TokenBundle{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return TokenBundle{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 400 || resp.StatusCode == 403 || resp.StatusCode == 404 {
		return TokenBundle{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TokenBundle{}, false, fmt.Errorf("OpenAI device poll returned HTTP %d", resp.StatusCode)
	}
	var code struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&code); err != nil {
		return TokenBundle{}, false, err
	}
	if code.AuthorizationCode == "" || code.CodeVerifier == "" {
		return TokenBundle{}, false, nil
	}
	tok, err := postForm(ctx, legacyOAuthURL, url.Values{"grant_type": {"authorization_code"}, "code": {code.AuthorizationCode}, "redirect_uri": {legacyRedirectURI}, "client_id": {legacyClientID}, "code_verifier": {code.CodeVerifier}})
	if err != nil {
		return TokenBundle{}, false, err
	}
	defer tok.Body.Close()
	var raw tokenResponse
	if err := json.NewDecoder(tok.Body).Decode(&raw); err != nil {
		return TokenBundle{}, false, err
	}
	if raw.ExpiresIn <= 0 {
		raw.ExpiresIn = 3600
	}
	return TokenBundle{AccessToken: raw.AccessToken, RefreshToken: raw.RefreshToken, IDToken: raw.IDToken, ExpiresAt: time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)}, true, nil
}
func Refresh(ctx context.Context, refreshToken string) (TokenBundle, error) {
	return RefreshWithClient(ctx, refreshToken, legacyClientID)
}
func RefreshWithClient(ctx context.Context, refreshToken, clientID string) (TokenBundle, error) {
	if clientID == "" {
		clientID = legacyClientID
	}
	resp, err := postForm(ctx, tokenURL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID}, "resource": {resource}})
	if err != nil {
		return TokenBundle{}, fmt.Errorf("refresh OpenAI OAuth token: %w", err)
	}
	defer resp.Body.Close()
	var raw tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return TokenBundle{}, err
	}
	if raw.AccessToken == "" {
		return TokenBundle{}, errors.New("OpenAI OAuth refresh response did not include an access token")
	}
	if raw.RefreshToken == "" {
		raw.RefreshToken = refreshToken
	}
	if raw.ExpiresIn <= 0 {
		raw.ExpiresIn = 3600
	}
	return TokenBundle{AccessToken: raw.AccessToken, RefreshToken: raw.RefreshToken, IDToken: raw.IDToken, ClientID: clientID, Scope: raw.Scope, ExpiresAt: time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)}, nil
}
