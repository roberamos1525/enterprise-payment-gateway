#!/usr/bin/env bash
# Builds the binaries, starts the gateway with throwaway development secrets,
# runs curl-walkthrough.sh (which launches the webhook receiver), then stops
# everything. Run from the repository root:  bash examples/local-stack.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORKDIR="${WORKDIR:-$(mktemp -d)}"
PORT="${PORT:-18080}"
RECEIVER_PORT="${RECEIVER_PORT:-19090}"
EXE=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe" ;; esac

echo "building into $WORKDIR"
(cd "$ROOT" && go build -o "$WORKDIR/" ./cmd/gateway ./cmd/gatewayctl ./cmd/webhook-receiver)

# Throwaway secrets for a local run only. Never reuse them.
VAULT_KEY="$("$WORKDIR/gatewayctl$EXE" gen-key -id local)"
export GATEWAY_ADMIN_TOKEN="local-admin-$(date +%s)-0123456789abcdef"

GATEWAY_ENV=dev \
GATEWAY_LISTEN_ADDR="127.0.0.1:$PORT" \
GATEWAY_DB_PATH="$WORKDIR/gateway.db" \
GATEWAY_VAULT_KEYS="$VAULT_KEY" \
GATEWAY_FINGERPRINT_SECRET="local-fingerprint-secret-$(date +%s)" \
GATEWAY_ADMIN_TOKENS="local:$GATEWAY_ADMIN_TOKEN" \
GATEWAY_WEBHOOK_ALLOW_HTTP=true \
GATEWAY_WEBHOOK_ALLOW_PRIVATE=true \
GATEWAY_WEBHOOK_BASE_BACKOFF=1s \
  "$WORKDIR/gateway$EXE" > "$WORKDIR/gateway.log" 2>&1 &
GW_PID=$!
trap 'kill $GW_PID 2>/dev/null || true' EXIT

for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done

GATEWAY_URL="http://127.0.0.1:$PORT" \
RECEIVER_URL="http://127.0.0.1:$RECEIVER_PORT/hooks" \
RECEIVER_ADDR="127.0.0.1:$RECEIVER_PORT" \
RECEIVER_BIN="$WORKDIR/webhook-receiver$EXE" \
WORKDIR="$WORKDIR" \
  bash "$ROOT/examples/curl-walkthrough.sh"

if grep -q "4242424242424242" "$WORKDIR/gateway.log"; then
  echo "ERROR: raw PAN found in gateway log" >&2
  exit 1
fi
echo "gateway log contains no raw PAN; logs in $WORKDIR"
