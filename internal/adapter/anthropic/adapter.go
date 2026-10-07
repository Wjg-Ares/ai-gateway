package anthropic

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Wjg-Ares/ai-gateway/internal/adapter"
	"github.com/Wjg-Ares/ai-gateway/internal/provider"
)

type Adapter struct{}

var client = &http.Client{
	Timeout: adapter.UpstreamTimeout(),
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (Adapter) Protocol() provider.Protocol { return provider.ProtocolAnthropic }

func (Adapter) Forward(ctx context.Context, upstream provider.Config, credential provider.Credential, apiPath string, body []byte, headers http.Header) (*http.Response, error) {
	endpoint := strings.TrimRight(upstream.BaseURL, "/") + apiPath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Anthropic upstream request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", credential.APIKey)
	version := headers.Get("anthropic-version")
	if version == "" {
		version = "2023-06-01"
	}
	request.Header.Set("anthropic-version", version)
	for _, name := range []string{"anthropic-beta", "anthropic-dangerous-direct-browser-access", "accept", "user-agent"} {
		if value := headers.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send Anthropic upstream request: %w", err)
	}
	return response, nil
}
