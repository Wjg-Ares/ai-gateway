package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Wjg-Ares/ai-gateway/internal/router"
)

// anthropicMessagesRequest captures the public Anthropic Messages fields used
// for validation and routing. The original body is otherwise passed through.
type anthropicMessagesRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func anthropicMessagesHandler(requestRouter router.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body is too large")
			} else {
				writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body could not be read")
			}
			return
		}
		var request anthropicMessagesRequest
		if err := json.Unmarshal(body, &request); err != nil || strings.TrimSpace(request.Model) == "" {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body must contain a valid model")
			return
		}

		response, err := requestRouter.Forward(r.Context(), request.Model, r.URL.Path, body, r.Header)
		if err != nil {
			var routeErr *router.RouteError
			if errors.As(err, &routeErr) {
				kind := "invalid_request_error"
				if routeErr.Status >= http.StatusInternalServerError {
					kind = "api_error"
				}
				writeAnthropicError(w, routeErr.Status, kind, routeErr.Message)
				return
			}
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed")
			return
		}
		defer response.Body.Close()
		copyResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		copyResponseBody(w, response.Body)
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		if strings.EqualFold(name, "Connection") || strings.EqualFold(name, "Keep-Alive") || strings.EqualFold(name, "Proxy-Authenticate") || strings.EqualFold(name, "Proxy-Authorization") || strings.EqualFold(name, "Te") || strings.EqualFold(name, "Trailer") || strings.EqualFold(name, "Transfer-Encoding") || strings.EqualFold(name, "Upgrade") {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func copyResponseBody(w http.ResponseWriter, body io.Reader) {
	buffer := make([]byte, 32*1024)
	flusher, canFlush := w.(http.Flusher)
	for {
		count, err := body.Read(buffer)
		if count > 0 {
			if _, writeErr := w.Write(buffer[:count]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
