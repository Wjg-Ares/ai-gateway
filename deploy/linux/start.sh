#!/bin/sh
set -eu

APP_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ENV_FILE="$APP_DIR/ai-gateway.env"

if [ ! -f "$ENV_FILE" ]; then
    cp "$APP_DIR/ai-gateway.env.example" "$ENV_FILE"
    chmod 600 "$ENV_FILE"
    printf 'Created %s. Edit it with your gateway and DeepSeek keys, then run this script again.\n' "$ENV_FILE"
    exit 1
fi

set -a
. "$ENV_FILE"
set +a

: "${AI_GATEWAY_API_KEY:?Set AI_GATEWAY_API_KEY in ai-gateway.env}"
: "${DEEPSEEK_API_KEY:?Set DEEPSEEK_API_KEY in ai-gateway.env}"
export AI_GATEWAY_CONFIG="${AI_GATEWAY_CONFIG:-$APP_DIR/ai-gateway.json}"

case "$(uname -m)" in
    x86_64|amd64) BINARY="$APP_DIR/ai-gateway-linux-amd64" ;;
    aarch64|arm64) BINARY="$APP_DIR/ai-gateway-linux-arm64" ;;
    *) printf 'Unsupported server architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

if [ ! -x "$BINARY" ]; then
    printf 'Binary not found or not executable: %s\n' "$BINARY" >&2
    printf 'Upload the matching Linux binary and run chmod +x on it.\n' >&2
    exit 1
fi

exec "$BINARY"
