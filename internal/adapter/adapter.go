package adapter

import (
	"context"
	"errors"
	"net/http"

	"github.com/Wjg-Ares/ai-gateway/internal/provider"
)

var ErrNotImplemented = errors.New("outbound adapter is not implemented")

type Adapter interface {
	Protocol() provider.Protocol
	Forward(ctx context.Context, upstream provider.Config, credential provider.Credential, apiPath string, body []byte, headers http.Header) (*http.Response, error)
}
