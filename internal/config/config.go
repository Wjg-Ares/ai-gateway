package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/Wjg-Ares/ai-gateway/internal/provider"
)

type Config struct {
	ListenAddr       string                     `json:"listen_addr"`
	GatewayAPIKeyEnv string                     `json:"gateway_api_key_env"`
	AdminAPIKeyEnv   string                     `json:"admin_api_key_env,omitempty"`
	AdminPasswordEnv string                     `json:"admin_password_env,omitempty"`
	DatabaseURLEnv   string                     `json:"database_url_env,omitempty"`
	DatabaseSchema   string                     `json:"database_schema,omitempty"`
	Models           map[string]ModelRoute      `json:"models"`
	Providers        map[string]provider.Config `json:"providers"`
}

type ModelRoute struct {
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
	// MaxConcurrency limits requests using this public model alias. A value of
	// zero uses the provider default (OpenAI-compatible GPT routes default to 1).
	MaxConcurrency int `json:"max_concurrency,omitempty"`
}

func LoadFile(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	cfg, err := Decode(file)
	if err != nil {
		return Config{}, err
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:19527"
	}
	if cfg.DatabaseURLEnv == "" {
		cfg.DatabaseURLEnv = "AI_GATEWAY_DATABASE_URL"
	}
	if cfg.AdminAPIKeyEnv == "" {
		cfg.AdminAPIKeyEnv = "AI_GATEWAY_ADMIN_KEY"
	}
	if cfg.AdminPasswordEnv == "" {
		cfg.AdminPasswordEnv = "AI_GATEWAY_ADMIN_PASSWORD"
	}
	if cfg.DatabaseSchema == "" {
		cfg.DatabaseSchema = "ai_gateway"
	}
	return cfg, nil
}

func Decode(r io.Reader) (Config, error) {
	var cfg Config
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	return cfg, nil
}
