# AI Gateway

A self-hosted AI API gateway for centrally managing access to multiple model providers, including OpenAI, Google Gemini, and Anthropic Claude.

## Project status

The Anthropic Messages API can now be routed to Anthropic-compatible and
OpenAI-compatible upstreams, including streaming responses and token-count
requests. Gemini outbound adapters remain a placeholder.

## Goals

- Provide one API endpoint for applications to access supported AI providers.
- Keep provider credentials on the gateway server.
- Manage gateway clients and their access centrally.
- Support usage tracking and operational controls as the project evolves.

The PostgreSQL schema for the database-backed rollout lives in
[`deploy/postgres/migrations/001_initial_schema.sql`](deploy/postgres/migrations/001_initial_schema.sql).
Database mode is optional: when `AI_GATEWAY_DATABASE_URL` is unset, the gateway
continues to use the JSON configuration and static environment API key.

## Protocol structure

- **Inbound:** Anthropic Messages API (`POST /v1/messages`), intended for Claude Code CLI clients.
- **Outbound adapter boundaries:** Anthropic, OpenAI-compatible, and Google Gemini.
- **Provider configuration:** Claude, DeepSeek, Volcengine, Kimi, OpenAI, and Gemini are represented as configured providers. DeepSeek can use the Anthropic-compatible endpoint `https://api.deepseek.com/anthropic`; configure the exact endpoint and model IDs provided by each vendor.

Inbound protocol handling is separate from outbound protocol adapters. For example, the Anthropic inbound route is not the same component as the Anthropic outbound adapter.

## Configuration

Start from [`configs/ai-gateway.example.json`](configs/ai-gateway.example.json). It contains model IDs and provider endpoint examples, not credentials. Set `AI_GATEWAY_CONFIG` to use another config file. Set the gateway client key using the environment variable named by `gateway_api_key_env`; set each upstream key using the variable named by that provider's `api_key_env`. Never commit actual keys.

The DeepSeek entry uses its Anthropic-compatible base URL. The Volcengine and Kimi URLs are placeholders and must be replaced with the exact endpoints provided for your accounts.

## Build and run

```powershell
go run ./cmd/ai-gateway
```

Build a Windows executable locally:

```powershell
go build -o .\bin\ai-gateway.exe ./cmd/ai-gateway
```

Cross-compile a Linux amd64 executable for a Debian/Ubuntu server:

```powershell
$env:GOOS = "linux"
$env:GOARCH = "amd64" # Set to arm64 for an ARM64 server.
go build -o .\bin\ai-gateway ./cmd/ai-gateway
```

The executable defaults to `127.0.0.1:19527`. `GET /healthz` returns a health response. `POST /v1/messages` and `POST /v1/messages/count_tokens` require the gateway key in `x-api-key` or `Authorization: Bearer ...`. The gateway resolves the public model alias, changes the model to its configured upstream name, then proxies the Anthropic request and response. Streaming bytes are passed through and flushed as they arrive. Anthropic-version and beta headers are forwarded; the provider API key is supplied by the gateway and is not forwarded from the client.

Before starting, set the gateway key and provider key in the server environment. For example, with the example config:

```sh
export AI_GATEWAY_API_KEY='choose-a-long-random-client-key'
export DEEPSEEK_API_KEY='your-deepseek-key'
./ai-gateway
```

To enable the first database-backed migration, set a PostgreSQL DSN in
`AI_GATEWAY_DATABASE_URL`. Database rows in `ai_gateway.models` override a
same-named JSON model route. Rows in `ai_gateway.gateway_api_keys` are accepted
alongside the existing static gateway key, so the rollout can be staged without
invalidating current clients.

When database mode is enabled, the built-in login page is available at `/admin/`
on the same listener. The service bootstraps the `admin` account from
`AI_GATEWAY_ADMIN_PASSWORD` (falling back to the old `AI_GATEWAY_ADMIN_KEY`
value during migration). Run the migration again after upgrading so the
`users.password_hash` column exists. Administrators can register users, add
models, add encrypted provider keys, assign shared model keys, and create
client gateway keys. Ordinary users can only manage the provider keys they
entered themselves. A generated client key is shown only once.

One provider/API or OAuth key can be associated with several model aliases.
The admin page supports multi-selecting models when saving a key; rerun the
PostgreSQL migration after upgrading so the `model_key_models` association
table is created and existing one-model links are preserved.

The admin page also includes a ChatGPT/Codex device-login button. It exchanges
the device authorization for an access/refresh-token bundle and encrypts that
bundle in `model_keys`; it does not read browser cookies. When a database
gateway key is used, the router selects the user's assigned API/OAuth key for
the requested model. Codex OAuth requests use the OpenAI Responses API with a
Bearer access token, and refresh the stored bundle when it is close to expiry.

Model routes can set `max_concurrency`. A value of `1` allows only one active
request for that public model alias; a second request receives HTTP 429 until
the first response closes. The built-in `openai-direct` provider defaults to
`1` when the field is omitted, so GPT routes are single-user by default. This
limit is held through streaming responses and is released when the response
ends or the client disconnects.

Use `deepseek-team` as the `model` value for a first DeepSeek check. The sample listener binds to localhost; put a TLS reverse proxy in front for remote Claude Code clients. Do not expose the plain HTTP listener to an untrusted network. The OpenAI-compatible adapter is used for providers such as Ark, Kimi, and OpenAI. ChatGPT web-account cookies are not read; the admin page uses the documented Codex device authorization flow when binding a ChatGPT account.

For a ready-to-upload Linux DeepSeek test setup (minimal config, environment template, start script, and server commands), see [`deploy/linux/README.md`](deploy/linux/README.md).

Example request from the gateway server:

```sh
curl -N http://127.0.0.1:19527/v1/messages \
  -H "x-api-key: $AI_GATEWAY_API_KEY" \
  -H "content-type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"deepseek-team","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"Reply with OK"}]}'
```

Claude Code can use the same listener by setting `ANTHROPIC_BASE_URL` to the gateway URL, `ANTHROPIC_AUTH_TOKEN` to the gateway key, and `ANTHROPIC_MODEL` to a configured model alias such as `deepseek-team`.

Outbound provider requests have a two-minute timeout by default, including
streaming responses. Set `AI_GATEWAY_UPSTREAM_TIMEOUT` to another Go duration
(for example, `90s` or `5m`) when a deployment needs a different bound. This
prevents a stalled upstream connection from holding a concurrency slot forever.

For a short diagnostic run only, set `AI_GATEWAY_DISABLE_CONCURRENCY_LIMITS=1`
to bypass the gateway's model and provider-key concurrency guards. This does
not bypass rate limits enforced by an upstream provider; remove the variable
after testing.

## Development

The gateway is planned to be implemented in Go and packaged for Debian and Ubuntu servers.

## License

License has not been selected yet.
