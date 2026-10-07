package router

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wjg-Ares/ai-gateway/internal/config"
	"github.com/Wjg-Ares/ai-gateway/internal/provider"
)

type testAdapter struct{}

func (testAdapter) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

func (testAdapter) Forward(context.Context, provider.Config, provider.Credential, string, []byte, http.Header) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("ok")),
	}, nil
}

type recordingAdapter struct{ apiKey string }

func (a *recordingAdapter) Protocol() provider.Protocol { return provider.ProtocolOpenAI }

func (a *recordingAdapter) Forward(_ context.Context, _ provider.Config, credential provider.Credential, _ string, _ []byte, _ http.Header) (*http.Response, error) {
	a.apiKey = credential.APIKey
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

type testCredentialResolver struct{}

func (testCredentialResolver) ResolveModelCredential(context.Context, int64, string) (provider.Credential, bool, error) {
	return provider.Credential{APIKey: "database-key", AuthType: "api_key"}, true, nil
}

type keyedCredentialResolver struct{}

func (keyedCredentialResolver) ResolveModelCredential(context.Context, int64, string) (provider.Credential, bool, error) {
	return provider.Credential{APIKey: "database-key", AuthType: "api_key", KeyID: 42, MaxConcurrency: 1}, true, nil
}

func TestOpenAIModelAllowsOnlyOneActiveResponse(t *testing.T) {
	t.Setenv("TEST_ROUTER_KEY", "test-key")
	router := New(config.Config{
		Models: map[string]config.ModelRoute{
			"gpt-team": {Provider: "openai-direct", UpstreamModel: "gpt-test"},
		},
		Providers: map[string]provider.Config{
			"openai-direct": {Protocol: provider.ProtocolOpenAI, BaseURL: "https://example.invalid/v1", APIKeyEnv: "TEST_ROUTER_KEY"},
		},
	}, testAdapter{})

	body := []byte(`{"model":"gpt-team","messages":[]}`)
	first, err := router.Forward(context.Background(), "gpt-team", "/v1/messages", body, nil)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	defer first.Body.Close()

	if _, err := router.Forward(context.Background(), "gpt-team", "/v1/messages", body, nil); err == nil {
		t.Fatal("second request unexpectedly acquired the model")
	} else {
		routeErr, ok := err.(*RouteError)
		if !ok || routeErr.Status != http.StatusTooManyRequests {
			t.Fatalf("second request error = %#v, want 429 RouteError", err)
		}
	}

	if err := first.Body.Close(); err != nil {
		t.Fatalf("close first response: %v", err)
	}
	third, err := router.Forward(context.Background(), "gpt-team", "/v1/messages", body, nil)
	if err != nil {
		t.Fatalf("request after release failed: %v", err)
	}
	_ = third.Body.Close()
}

func TestNonOpenAIModelIsUnlimitedByDefault(t *testing.T) {
	t.Setenv("TEST_ROUTER_KEY", "test-key")
	router := New(config.Config{
		Models: map[string]config.ModelRoute{
			"deepseek-team": {Provider: "deepseek", UpstreamModel: "deepseek-chat"},
		},
		Providers: map[string]provider.Config{
			"deepseek": {Protocol: provider.ProtocolOpenAI, BaseURL: "https://example.invalid/v1", APIKeyEnv: "TEST_ROUTER_KEY"},
		},
	}, testAdapter{})

	body := []byte(`{"model":"deepseek-team","messages":[]}`)
	first, err := router.Forward(context.Background(), "deepseek-team", "/v1/messages", body, nil)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	defer first.Body.Close()
	second, err := router.Forward(context.Background(), "deepseek-team", "/v1/messages", body, nil)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	_ = second.Body.Close()
}

func TestDatabaseCredentialOverridesEnvironmentCredential(t *testing.T) {
	t.Setenv("TEST_ROUTER_KEY", "environment-key")
	adapter := &recordingAdapter{}
	r := New(config.Config{
		Models:    map[string]config.ModelRoute{"deepseek-team": {Provider: "deepseek", UpstreamModel: "deepseek-chat"}},
		Providers: map[string]provider.Config{"deepseek": {Protocol: provider.ProtocolOpenAI, BaseURL: "https://example.invalid/v1", APIKeyEnv: "TEST_ROUTER_KEY"}},
	}, adapter)
	r.SetCredentialResolver(testCredentialResolver{})
	response, err := r.Forward(WithRequestUserID(context.Background(), 7), "deepseek-team", "/v1/messages", []byte(`{"model":"deepseek-team"}`), nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = response.Body.Close()
	if adapter.apiKey != "database-key" {
		t.Fatalf("credential = %q, want database-key", adapter.apiKey)
	}
}

func TestDatabaseKeyLimitAppliesAcrossModelAliases(t *testing.T) {
	t.Setenv("TEST_ROUTER_KEY", "environment-key")
	r := New(config.Config{
		Models: map[string]config.ModelRoute{
			"alias-a": {Provider: "deepseek", UpstreamModel: "deepseek-a"},
			"alias-b": {Provider: "deepseek", UpstreamModel: "deepseek-b"},
		},
		Providers: map[string]provider.Config{"deepseek": {Protocol: provider.ProtocolOpenAI, BaseURL: "https://example.invalid/v1", APIKeyEnv: "TEST_ROUTER_KEY"}},
	}, testAdapter{})
	r.SetCredentialResolver(keyedCredentialResolver{})
	ctx := WithRequestUserID(context.Background(), 7)
	body := []byte(`{"model":"alias-a","messages":[]}`)
	first, err := r.Forward(ctx, "alias-a", "/v1/messages", body, nil)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	defer first.Body.Close()
	if _, err := r.Forward(ctx, "alias-b", "/v1/messages", body, nil); err == nil {
		t.Fatal("same key was used concurrently through a second alias")
	} else if routeErr, ok := err.(*RouteError); !ok || routeErr.Status != http.StatusTooManyRequests {
		t.Fatalf("second request error = %#v, want 429 RouteError", err)
	}
	if err := first.Body.Close(); err != nil {
		t.Fatalf("close first response: %v", err)
	}
	second, err := r.Forward(ctx, "alias-b", "/v1/messages", body, nil)
	if err != nil {
		t.Fatalf("request after release failed: %v", err)
	}
	_ = second.Body.Close()
}
