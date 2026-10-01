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
	Models           map[string]ModelRoute      `json:"models"`
	Providers        map[string]provider.Config `json:"providers"`
}

type ModelRoute struct {
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
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
