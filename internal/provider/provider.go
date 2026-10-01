package provider

import "fmt"

type Protocol string

const (
	ProtocolAnthropic Protocol = "anthropic"
	ProtocolOpenAI    Protocol = "openai"
	ProtocolGemini    Protocol = "gemini"
)

// Config describes an upstream provider. APIKeyEnv names the environment
// variable that will supply the credential; the credential itself is never
// stored in this configuration type or example file.
type Config struct {
	Protocol  Protocol `json:"protocol"`
	BaseURL   string   `json:"base_url"`
	APIKeyEnv string   `json:"api_key_env"`
}

type Credential struct {
	APIKey string
}

func (c Config) ResolveCredential(lookupEnv func(string) (string, bool)) (Credential, error) {
	if c.APIKeyEnv == "" {
		return Credential{}, fmt.Errorf("provider API key environment variable is not configured")
	}
	key, ok := lookupEnv(c.APIKeyEnv)
	if !ok || key == "" {
		return Credential{}, fmt.Errorf("provider API key environment variable %q is not set", c.APIKeyEnv)
	}
	return Credential{APIKey: key}, nil
}
