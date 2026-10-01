package gemini

import (
	"context"
	"net/http"

	"github.com/Wjg-Ares/ai-gateway/internal/adapter"
	"github.com/Wjg-Ares/ai-gateway/internal/provider"
)

type Adapter struct{}

func (Adapter) Protocol() provider.Protocol { return provider.ProtocolGemini }

func (Adapter) Forward(context.Context, provider.Config, provider.Credential, string, []byte, http.Header) (*http.Response, error) {
	return nil, adapter.ErrNotImplemented
}
