package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/Wjg-Ares/ai-gateway/internal/adapter/anthropic"
	"github.com/Wjg-Ares/ai-gateway/internal/adapter/gemini"
	"github.com/Wjg-Ares/ai-gateway/internal/adapter/openai"
	"github.com/Wjg-Ares/ai-gateway/internal/admin"
	"github.com/Wjg-Ares/ai-gateway/internal/config"
	"github.com/Wjg-Ares/ai-gateway/internal/database"
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
	var gatewayCredential string
	if cfg.GatewayAPIKeyEnv != "" {
		gatewayCredential, _ = os.LookupEnv(cfg.GatewayAPIKeyEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	databaseStore, err := openOptionalDatabase(ctx, cfg)
	cancel()
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	if databaseStore != nil {
		defer databaseStore.Close()
		encryptionSecret, _ := os.LookupEnv("AI_GATEWAY_KEY_ENCRYPTION_SECRET")
		databaseStore.SetEncryptionSecret(encryptionSecret)
		adminPassword, _ := os.LookupEnv(cfg.AdminPasswordEnv)
		if adminPassword == "" {
			// Backward-compatible bootstrap: existing deployments already have
			// AI_GATEWAY_ADMIN_KEY configured. A dedicated password variable is
			// preferred, but the old value can initialize the admin account.
			adminPassword, _ = os.LookupEnv(cfg.AdminAPIKeyEnv)
		}
		if adminPassword != "" {
			if err := databaseStore.EnsureAdmin(context.Background(), adminPassword); err != nil {
				log.Fatalf("ensure admin user: %v", err)
			}
		}
		databaseModels, err := databaseStore.LoadModels(context.Background())
		if err != nil {
			log.Fatalf("load database models: %v", err)
		}
		for name, model := range databaseModels {
			if cfg.Models == nil {
				cfg.Models = make(map[string]config.ModelRoute)
			}
			cfg.Models[name] = model
		}
	}
	if gatewayCredential == "" && databaseStore == nil {
		if cfg.GatewayAPIKeyEnv == "" {
			log.Fatal("gateway_api_key_env must be configured when database mode is disabled")
		}
		log.Fatalf("gateway API key environment variable %q is not set", cfg.GatewayAPIKeyEnv)
	}

	requestRouter := router.New(cfg, anthropic.Adapter{}, openai.Adapter{}, gemini.Adapter{})
	if databaseStore != nil {
		requestRouter.SetCredentialResolver(databaseStore)
	}
	var authenticator gateway.APIKeyAuthenticator = gateway.NewStaticAuthenticator(gatewayCredential)
	var adminHandler http.Handler
	if databaseStore != nil {
		authenticator = gateway.NewCompositeAuthenticator(authenticator, databaseStore)
		encryptionSecret, _ := os.LookupEnv("AI_GATEWAY_KEY_ENCRYPTION_SECRET")
		providerNames := make([]string, 0, len(cfg.Providers))
		for name := range cfg.Providers {
			providerNames = append(providerNames, name)
		}
		sort.Strings(providerNames)
		adminHandler = admin.New(databaseStore, encryptionSecret, providerNames)
	}
	server := gateway.NewServerWithAdmin(cfg.ListenAddr, authenticator, requestRouter, adminHandler)

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

func openOptionalDatabase(ctx context.Context, cfg config.Config) (*database.Store, error) {
	if cfg.DatabaseURLEnv == "" {
		return nil, nil
	}
	dsn, ok := os.LookupEnv(cfg.DatabaseURLEnv)
	if !ok || dsn == "" {
		return nil, nil
	}
	log.Printf("opening PostgreSQL database from %s", cfg.DatabaseURLEnv)
	return database.Open(ctx, dsn, cfg.DatabaseSchema)
}
