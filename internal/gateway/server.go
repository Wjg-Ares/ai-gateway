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
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler)
	mux.Handle("POST /v1/messages", authenticate(gatewayAPIKey, http.HandlerFunc(anthropicMessagesHandler(requestRouter))))
	mux.Handle("POST /v1/messages/count_tokens", authenticate(gatewayAPIKey, http.HandlerFunc(anthropicMessagesHandler(requestRouter))))

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

func authenticate(expected string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("x-api-key")
		if provided == "" {
			scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
			if found && strings.EqualFold(scheme, "Bearer") {
				provided = strings.TrimSpace(token)
			}
		}
		if expected == "" || len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid or missing gateway API key")
			return
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
