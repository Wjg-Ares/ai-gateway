# Linux quick test (Anthropic-compatible DeepSeek)

Upload these files into the same server directory as the Linux binary:

- `ai-gateway.json`
- `ai-gateway.env.example`
- `start.sh`
- `ai-gateway-linux-amd64` for `x86_64`, or `ai-gateway-linux-arm64` for `aarch64`

Then on the server:

```sh
cd /path/to/gateway
chmod +x start.sh ai-gateway-linux-amd64
./start.sh
```

The first run creates `ai-gateway.env` and exits. Edit that file and replace both example values:

- `AI_GATEWAY_API_KEY`: a key you create for clients connecting to your gateway.
- `DEEPSEEK_API_KEY`: your DeepSeek API key.

Save the file, then restrict its permissions and start the gateway:

```sh
chmod 600 ai-gateway.env
./start.sh
```

The service listens on `127.0.0.1:19527`. In another server shell, test it:

```sh
. ./ai-gateway.env
curl -N http://127.0.0.1:19527/v1/messages \
  -H "x-api-key: $AI_GATEWAY_API_KEY" \
  -H "content-type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"deepseek-team","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"Reply with OK"}]}'
```

Press `Ctrl-C` to stop the service. This quick-test setup binds only to localhost. Use a TLS reverse proxy before connecting Claude Code from another computer; do not expose this plain HTTP port to an untrusted network.
