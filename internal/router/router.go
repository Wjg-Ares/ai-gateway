package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

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

// ModelLister exposes the public model aliases configured on the gateway.
// It is used by clients such as CC Switch to populate their model picker.
type ModelLister interface {
	ListModels(context.Context, int64) ([]string, error)
}

// CredentialResolver supplies a database-managed upstream key for the
// authenticated gateway user and public model. The bool reports whether a
// database key was found; callers may then fall back to the static env key.
type CredentialResolver interface {
	ResolveModelCredential(context.Context, int64, string) (provider.Credential, bool, error)
}

type requestUserIDContextKey struct{}

func WithRequestUserID(ctx context.Context, userID int64) context.Context {
	if userID <= 0 {
		return ctx
	}
	return context.WithValue(ctx, requestUserIDContextKey{}, userID)
}

func RequestUserID(ctx context.Context) (int64, bool) {
	userID, ok := ctx.Value(requestUserIDContextKey{}).(int64)
	return userID, ok && userID > 0
}

type ConfigRouter struct {
	config             config.Config
	adapters           map[provider.Protocol]adapter.Adapter
	limits             map[string]*modelLimiter
	keyLimits          map[int64]*modelLimiter
	keyLimitsMu        sync.Mutex
	credentialResolver CredentialResolver
	disableConcurrency bool
}

func New(cfg config.Config, adapters ...adapter.Adapter) *ConfigRouter {
	registry := make(map[provider.Protocol]adapter.Adapter, len(adapters))
	for _, item := range adapters {
		registry[item.Protocol()] = item
	}
	limits := make(map[string]*modelLimiter)
	for publicModel, route := range cfg.Models {
		if limit := modelConcurrencyLimit(cfg, route); limit > 0 {
			limits[publicModel] = newModelLimiter(limit)
		}
	}
	disableConcurrency := strings.EqualFold(strings.TrimSpace(os.Getenv("AI_GATEWAY_DISABLE_CONCURRENCY_LIMITS")), "1") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("AI_GATEWAY_DISABLE_CONCURRENCY_LIMITS")), "true")
	return &ConfigRouter{config: cfg, adapters: registry, limits: limits, keyLimits: make(map[int64]*modelLimiter), disableConcurrency: disableConcurrency}
}

func (r *ConfigRouter) SetCredentialResolver(resolver CredentialResolver) {
	r.credentialResolver = resolver
}

func (r *ConfigRouter) ListModels(_ context.Context, _ int64) ([]string, error) {
	models := make([]string, 0, len(r.config.Models))
	for name := range r.config.Models {
		models = append(models, name)
	}
	sort.Strings(models)
	return models, nil
}

func (r *ConfigRouter) Forward(ctx context.Context, publicModel, apiPath string, body []byte, headers http.Header) (*http.Response, error) {
	modelRoute, ok := r.config.Models[publicModel]
	if !ok {
		return nil, &RouteError{Status: http.StatusNotFound, Message: fmt.Sprintf("model %q is not configured", publicModel)}
	}
	if !r.disableConcurrency {
		if limiter := r.limits[publicModel]; limiter != nil {
			release, ok := limiter.acquire()
			if !ok {
				return nil, &RouteError{Status: http.StatusTooManyRequests, Message: fmt.Sprintf("model %q is currently in use; try again after the active request finishes", publicModel)}
			}
			response, err := r.forwardWithRoute(ctx, publicModel, apiPath, body, headers, modelRoute)
			if err != nil {
				release()
				return nil, err
			}
			if response == nil || response.Body == nil {
				release()
				return response, nil
			}
			response.Body = &releaseBody{ReadCloser: response.Body, release: release}
			return response, nil
		}
	}
	return r.forwardWithRoute(ctx, publicModel, apiPath, body, headers, modelRoute)
}

func (r *ConfigRouter) forwardWithRoute(ctx context.Context, publicModel, apiPath string, body []byte, headers http.Header, modelRoute config.ModelRoute) (*http.Response, error) {
	upstream, ok := r.config.Providers[modelRoute.Provider]
	if !ok {
		return nil, fmt.Errorf("provider %q referenced by model %q is not configured", modelRoute.Provider, publicModel)
	}
	selected := r.adapters[upstream.Protocol]
	if selected == nil {
		return nil, &RouteError{Status: http.StatusNotImplemented, Message: fmt.Sprintf("outbound protocol %q is not implemented", upstream.Protocol)}
	}
	credential, envErr := upstream.ResolveCredential(os.LookupEnv)
	userScoped := false
	if userID, ok := RequestUserID(ctx); ok && r.credentialResolver != nil {
		userScoped = true
		resolved, found, resolveErr := r.credentialResolver.ResolveModelCredential(ctx, userID, publicModel)
		if resolveErr != nil {
			return nil, &RouteError{Status: http.StatusInternalServerError, Message: "failed to load the assigned upstream credential"}
		}
		if found {
			credential = resolved
		} else {
			// A database-issued user key must not silently fall back to a
			// process-wide provider secret; otherwise an unassigned user could
			// consume the gateway owner's upstream account.
			credential = provider.Credential{}
		}
	}
	if credential.APIKey == "" {
		if userScoped || envErr != nil {
			return nil, &RouteError{Status: http.StatusInternalServerError, Message: "upstream provider credential is not configured on the gateway"}
		}
		return nil, &RouteError{Status: http.StatusInternalServerError, Message: "upstream provider credential is not configured on the gateway"}
	}
	if credential.AuthType == "codex_oauth" && !strings.EqualFold(modelRoute.Provider, "openai-direct") {
		return nil, &RouteError{Status: http.StatusBadRequest, Message: "Codex OAuth credentials can only be used with the openai-direct provider"}
	}
	var releaseKey func()
	if credential.KeyID > 0 && !r.disableConcurrency {
		limit := credential.MaxConcurrency
		if limit <= 0 {
			limit = 1
		}
		release, acquired := r.credentialLimiter(credential.KeyID, limit).acquire()
		if !acquired {
			return nil, &RouteError{Status: http.StatusTooManyRequests, Message: "model key is currently in use; try again after the active request finishes"}
		}
		releaseKey = release
	}
	forwardBody, err := replaceModel(body, modelRoute.UpstreamModel)
	if err != nil {
		if releaseKey != nil {
			releaseKey()
		}
		return nil, &RouteError{Status: http.StatusBadRequest, Message: "request body must be a JSON object with a string model"}
	}
	response, err := selected.Forward(ctx, upstream, credential, apiPath, forwardBody, headers)
	if err != nil {
		if releaseKey != nil {
			releaseKey()
		}
		return nil, err
	}
	if response == nil || response.Body == nil {
		if releaseKey != nil {
			releaseKey()
		}
		return response, nil
	}
	if releaseKey != nil {
		response.Body = &releaseBody{ReadCloser: response.Body, release: releaseKey}
	}
	return response, nil
}

func (r *ConfigRouter) credentialLimiter(keyID int64, limit int) *modelLimiter {
	r.keyLimitsMu.Lock()
	defer r.keyLimitsMu.Unlock()
	if limiter := r.keyLimits[keyID]; limiter != nil {
		return limiter
	}
	limiter := newModelLimiter(limit)
	r.keyLimits[keyID] = limiter
	return limiter
}

// modelConcurrencyLimit returns an explicit limit when configured. For the
// built-in OpenAI provider, one request at a time is the safe default because
// a GPT/Codex account is intended to be shared by only one active caller.
func modelConcurrencyLimit(cfg config.Config, route config.ModelRoute) int {
	if route.MaxConcurrency > 0 {
		return route.MaxConcurrency
	}
	upstream, ok := cfg.Providers[route.Provider]
	if ok && upstream.Protocol == provider.ProtocolOpenAI && strings.EqualFold(route.Provider, "openai-direct") {
		return 1
	}
	return 0
}

type modelLimiter struct {
	slots chan struct{}
}

func newModelLimiter(limit int) *modelLimiter {
	return &modelLimiter{slots: make(chan struct{}, limit)}
}

func (l *modelLimiter) acquire() (func(), bool) {
	select {
	case l.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-l.slots }) }, true
	default:
		return nil, false
	}
}

type releaseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
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
