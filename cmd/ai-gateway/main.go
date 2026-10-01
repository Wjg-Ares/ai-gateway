package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Wjg-Ares/ai-gateway/internal/adapter/anthropic"
	"github.com/Wjg-Ares/ai-gateway/internal/adapter/gemini"
	"github.com/Wjg-Ares/ai-gateway/internal/adapter/openai"
	"github.com/Wjg-Ares/ai-gateway/internal/config"
	"github.com/Wjg-Ares/ai-gateway/internal/gateway"
	"github.com/Wjg-Ares/ai-gateway/internal/router"
)

func main() {
	configPath := os.Getenv("AI_GATEWAY_CONFIG")
	if configPath == "" {
		configPath = "configs/ai-gateway.example.json"
	}

	cfg, err := config.LoadFile(configPath)
	if err != nil {
		log.Fatalf("load configuration: %v", err)
	}
	if cfg.GatewayAPIKeyEnv == "" {
		log.Fatal("gateway_api_key_env must be configured")
	}
	gatewayCredential, ok := os.LookupEnv(cfg.GatewayAPIKeyEnv)
	if !ok || gatewayCredential == "" {
		log.Fatalf("gateway API key environment variable %q is not set", cfg.GatewayAPIKeyEnv)
	}

	requestRouter := router.New(cfg, anthropic.Adapter{}, openai.Adapter{}, gemini.Adapter{})
	server := gateway.NewServer(cfg.ListenAddr, gatewayCredential, requestRouter)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("shutdown server: %v", err)
		}
	}()

	log.Printf("AI Gateway skeleton listening on %s", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
}
