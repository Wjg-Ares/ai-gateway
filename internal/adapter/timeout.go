package adapter

import (
	"os"
	"strings"
	"time"
)

const defaultUpstreamTimeout = 2 * time.Minute

// UpstreamTimeout bounds an outbound request, including a streaming response.
// Without a bound, a stalled upstream connection can hold a router concurrency
// slot forever and make later requests look like an endless 429 loop.
func UpstreamTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("AI_GATEWAY_UPSTREAM_TIMEOUT")); raw != "" {
		if timeout, err := time.ParseDuration(raw); err == nil && timeout > 0 {
			return timeout
		}
	}
	return defaultUpstreamTimeout
}
