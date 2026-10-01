# AI Gateway Structure Design

## Goal

Define a Go project structure for a self-hosted AI gateway with a provider-independent inbound API layer and separately extensible outbound protocol adapters. This phase creates the project skeleton and contracts only; it does not implement request forwarding.

## Current requirements

- The project is a self-hosted gateway for centrally managed access to multiple AI providers.
- The initial inbound protocol is Anthropic Messages API, because the team primarily uses Claude Code CLI.
- Inbound and outbound protocol handling must remain separate.
- Initial outbound protocol adapter families are Anthropic, OpenAI-compatible, and Google Gemini.
- Claude, DeepSeek, Volcengine, and Kimi are provider configurations. DeepSeek, Volcengine, and Kimi can use the Anthropic outbound adapter when configured with their Anthropic-compatible API endpoint, API key, and model mapping.
- Provider-specific settings such as `base_url`, credential, and model identifiers belong in configuration, not in protocol adapter code.
- This phase creates Go module metadata, configuration example, package layout, interface contracts, and documentation. No live API calls, persistence layer, admin UI, or billing are included.

## Recommended architecture

Use protocol adapters at both sides of the gateway. The HTTP/API layer parses and validates the Anthropic Messages request, resolves the requested public model alias, and passes a normalized internal request to the routing layer. The routing layer selects a configured provider and its outbound protocol adapter. Adapters are separate packages grouped by protocol, not vendor, so multiple providers can share one adapter. The response path is designed to translate the provider result back to the inbound Anthropic response format.

The first skeleton declares these boundaries without implementing forwarding. This avoids prematurely committing to detailed translation behavior while making the direction of dependencies explicit.

## Proposed layout

```text
cmd/ai-gateway/             executable entry point
internal/config/            configuration types and loading boundary
internal/gateway/           HTTP server, routes, inbound protocol boundary
internal/protocol/          normalized request/response contracts
internal/router/            model alias and provider selection boundary
internal/provider/           provider registry and provider definitions
internal/adapter/anthropic/  Anthropic outbound protocol adapter boundary
internal/adapter/openai/     OpenAI-compatible outbound adapter boundary
internal/adapter/gemini/     Gemini outbound adapter boundary
configs/                     example configuration
docs/                        project and design documentation
```

Keep the inbound Anthropic HTTP handling under `internal/gateway` (or a dedicated inbound package when implemented); do not conflate it with `internal/adapter/anthropic`, which represents the outbound protocol.

## Configuration direction

The example config should demonstrate public model aliases mapped to provider records. A provider record identifies its protocol, `base_url`, model name, and a credential reference supplied through an environment variable. It must not contain real credentials. An illustrative provider could be DeepSeek with protocol `anthropic` and base URL `https://api.deepseek.com/anthropic`; Volcengine and Kimi follow the same pattern if their chosen endpoints support the compatible protocol.

Exact configuration syntax is an implementation detail, but secrets must not be committed in plain text. The sample file should use environment-variable references or obvious placeholders.

## Interfaces and data flow

The normalized protocol package owns internal request/response types. The gateway depends on the router interface. The router depends on provider configuration and an outbound adapter interface. Protocol adapters translate between normalized types and their upstream protocol. Provider names and model aliases are configuration data rather than adapter implementations.

Planned flow:

```text
Claude Code CLI
  -> Anthropic Messages API inbound handler
  -> model alias resolver / router
  -> selected outbound protocol adapter
  -> configured provider endpoint
```

## Error handling and security direction

- Keep inbound protocol errors separate from upstream transport/protocol errors so later translation can return useful Anthropic-format errors.
- Do not log API keys or request bodies by default.
- Do not place real provider credentials in the repository or example config.
- Bind/listen defaults and deployment hardening will be specified when the executable server is implemented.

## Deferred scope

- Live Anthropic Messages handling and streaming.
- OpenAI and Gemini request/response translation.
- Authentication for gateway clients, quotas, usage accounting, persistence, and admin UI.
- Automatic retries/fallback, health checks, metrics, packaging, and apt repository publishing.

## Review checklist

- Does the inbound/outbound protocol distinction match the intended design?
- Are Anthropic, OpenAI-compatible, and Gemini the right initial outbound adapter families?
- Is a skeleton with config example and interface contracts the intended first implementation milestone?
