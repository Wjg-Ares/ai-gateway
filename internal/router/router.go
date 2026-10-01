package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/Wjg-Ares/ai-gateway/internal/adapter"
	"github.com/Wjg-Ares/ai-gateway/internal/config"
	"github.com/Wjg-Ares/ai-gateway/internal/provider"
)

type RouteError struct {
	Status  int
	Message string
}

func (e *RouteError) Error() string { return e.Message }

type Router interface {
	Forward(ctx context.Context, publicModel, apiPath string, body []byte, headers http.Header) (*http.Response, error)
}

type ConfigRouter struct {
	config   config.Config
	adapters map[provider.Protocol]adapter.Adapter
}

func New(cfg config.Config, adapters ...adapter.Adapter) *ConfigRouter {
	registry := make(map[provider.Protocol]adapter.Adapter, len(adapters))
	for _, item := range adapters {
		registry[item.Protocol()] = item
	}
	return &ConfigRouter{config: cfg, adapters: registry}
}

func (r *ConfigRouter) Forward(ctx context.Context, publicModel, apiPath string, body []byte, headers http.Header) (*http.Response, error) {
	modelRoute, ok := r.config.Models[publicModel]
	if !ok {
		return nil, &RouteError{Status: http.StatusNotFound, Message: fmt.Sprintf("model %q is not configured", publicModel)}
	}
	upstream, ok := r.config.Providers[modelRoute.Provider]
	if !ok {
		return nil, fmt.Errorf("provider %q referenced by model %q is not configured", modelRoute.Provider, publicModel)
	}
	selected := r.adapters[upstream.Protocol]
	if selected == nil {
		return nil, &RouteError{Status: http.StatusNotImplemented, Message: fmt.Sprintf("outbound protocol %q is not implemented", upstream.Protocol)}
	}
	credential, err := upstream.ResolveCredential(os.LookupEnv)
	if err != nil {
		return nil, &RouteError{Status: http.StatusInternalServerError, Message: "upstream provider credential is not configured on the gateway"}
	}
	forwardBody, err := replaceModel(body, modelRoute.UpstreamModel)
	if err != nil {
		return nil, &RouteError{Status: http.StatusBadRequest, Message: "request body must be a JSON object with a string model"}
	}
	return selected.Forward(ctx, upstream, credential, apiPath, forwardBody, headers)
}

func replaceModel(body []byte, model string) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, fmt.Errorf("decode messages request")
	}
	if model == "" {
		return body, nil
	}
	encodedModel, _ := json.Marshal(model)
	object["model"] = encodedModel
	return json.Marshal(object)
}
