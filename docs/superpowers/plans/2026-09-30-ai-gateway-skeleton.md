# AI Gateway Skeleton Implementation Plan

> **For agentic workers:** Execute this plan inline in the current session, following the approved design at `docs/superpowers/specs/2026-09-30-ai-gateway-structure-design.md`.

**Goal:** Create a compilable Go project skeleton with separate Anthropic inbound and multi-protocol outbound boundaries.

**Architecture:** Keep the HTTP entry boundary separate from outbound protocol adapters. Define normalized request/response contracts and configuration/provider types without making network calls or adding runtime behavior beyond a health endpoint if needed to make the executable observable.

**Tech Stack:** Go standard library only; YAML-free JSON example configuration to avoid a dependency before the configuration format is proven.

## Global Constraints

- Initial inbound protocol: Anthropic Messages API.
- Initial outbound adapter families: Anthropic, OpenAI-compatible, and Google Gemini.
- Provider settings and credentials are configuration; no credentials are committed.
- This milestone does not implement live model forwarding, persistence, admin UI, or billing.
- Do not add or run tests unless separately requested; use `go build ./...` to verify compilation.

---

## File map

- `go.mod`: module path and Go language version.
- `cmd/ai-gateway/main.go`: executable entry point.
- `internal/protocol/types.go`: normalized internal request/response types.
- `internal/provider/provider.go`: provider protocol and endpoint configuration types.
- `internal/config/config.go`: top-level configuration types and environment-secret resolution boundary.
- `internal/gateway/server.go`: inbound HTTP server and route registration boundary.
- `internal/gateway/anthropic_messages.go`: Anthropic inbound route placeholder and request type boundary.
- `internal/router/router.go`: model alias resolution and outbound adapter selection interface.
- `internal/adapter/adapter.go`: outbound adapter interface shared by protocol implementations.
- `internal/adapter/anthropic/adapter.go`: Anthropic outbound adapter placeholder.
- `internal/adapter/openai/adapter.go`: OpenAI-compatible outbound adapter placeholder.
- `internal/adapter/gemini/adapter.go`: Gemini outbound adapter placeholder.
- `configs/ai-gateway.example.json`: safe example configuration, with environment variable names instead of secrets.
- `README.md`: explain structure, config example, current scope, and build command.

## Steps

### 1. Initialize the Go module and normalized contracts

Create `go.mod` with module path `github.com/Wjg-Ares/ai-gateway` and the installed Go major/minor language version. Add `internal/protocol/types.go` with minimal provider-neutral message, request, response, and usage structures needed to define adapter boundaries. Keep fields limited to model, messages, generation options, content text, and usage; defer tools, images, and streaming.

### 2. Define provider configuration and secret references

Add `internal/provider/provider.go` with a protocol enum (`anthropic`, `openai`, `gemini`) and provider configuration fields for name, protocol, base URL, upstream model, and API key environment variable. Add `internal/config/config.go` for listener address, public model aliases, and provider records. Provide a loader signature but avoid adding a third-party parser dependency; use JSON in this milestone. Resolve secrets from environment at runtime and return a clear error when a configured variable is absent.

### 3. Define routing and outbound adapter contracts

Add an outbound adapter interface that consumes normalized requests and returns normalized responses. Add a router interface for resolving a public model alias to a provider and adapter. Create protocol-specific adapter packages with named types that satisfy the interface while returning an explicit `ErrNotImplemented`; do not make HTTP requests.

### 4. Define the Anthropic inbound HTTP boundary and executable

Add an HTTP server package with `/healthz` and `/v1/messages` route registration. `/v1/messages` should return a clear not-implemented response while preserving its intended Anthropic inbound route boundary. Add `cmd/ai-gateway/main.go` to load example/default configuration and start the HTTP server. Avoid implying model forwarding works.

### 5. Add configuration example and update README

Add JSON config with aliases for illustrative Claude, DeepSeek, Volcengine, Kimi, OpenAI, and Gemini providers. Show DeepSeek using `https://api.deepseek.com/anthropic` and protocol `anthropic`; use environment-variable names for all credentials. Update README with package map, build/run steps, and an explicit list of implemented versus planned behavior.

### 6. Verify compile and inspect staged scope

Run `gofmt` on Go files and `go build ./...`. Review `git diff` to ensure there are no real keys, accidental generated binaries, or claims that provider forwarding is implemented.
