package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wjg-Ares/ai-gateway/internal/router"
)

var requestSequence uint64

// anthropicMessagesRequest captures the public Anthropic Messages fields used
// for validation and routing. The original body is otherwise passed through.
type anthropicMessagesRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func anthropicMessagesHandler(requestRouter router.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := atomic.AddUint64(&requestSequence, 1)
		started := time.Now()
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
		log.Printf("gateway request id=%d start path=%s model=%s stream=%t body_bytes=%d", requestID, r.URL.Path, request.Model, request.Stream, len(body))

		response, err := requestRouter.Forward(r.Context(), request.Model, r.URL.Path, body, r.Header)
		if err != nil {
			log.Printf("gateway request id=%d route_error after=%s err=%v", requestID, time.Since(started), err)
			var routeErr *router.RouteError
			if errors.As(err, &routeErr) {
				kind := "invalid_request_error"
				if routeErr.Status == http.StatusTooManyRequests {
					kind = "rate_limit_error"
				}
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
		log.Printf("gateway request id=%d upstream_headers status=%d content_type=%s after=%s", requestID, response.StatusCode, response.Header.Get("Content-Type"), time.Since(started))
		copyResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		copyResponseBody(w, response.Body, func(first bool, bytesWritten int64, copyErr error) {
			if first {
				log.Printf("gateway request id=%d first_bytes after=%s bytes=%d", requestID, time.Since(started), bytesWritten)
			}
			if copyErr != nil {
				log.Printf("gateway request id=%d body_error after=%s bytes=%d err=%v", requestID, time.Since(started), bytesWritten, copyErr)
			} else {
				log.Printf("gateway request id=%d complete after=%s bytes=%d", requestID, time.Since(started), bytesWritten)
			}
		})
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

func copyResponseBody(w http.ResponseWriter, body io.Reader, done func(first bool, bytesWritten int64, copyErr error)) {
	buffer := make([]byte, 32*1024)
	flusher, canFlush := w.(http.Flusher)
	var total int64
	first := true
	for {
		count, err := body.Read(buffer)
		if count > 0 {
			if _, writeErr := w.Write(buffer[:count]); writeErr != nil {
				if done != nil {
					done(first, total, writeErr)
				}
				return
			}
			total += int64(count)
			if first && done != nil {
				done(true, total, nil)
				first = false
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if done != nil {
				done(false, total, func() error {
					if err == io.EOF {
						return nil
					}
					return err
				}())
			}
			return
		}
	}
}
