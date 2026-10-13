#!/usr/bin/env bash
# End-to-end walkthrough of the simulated gateway using curl only.
#
#   onboard merchant -> API key -> webhook endpoint -> authorize -> partial
#   capture -> refund -> settlement -> reconciliation -> webhook delivery
#
# Requirements: a running gateway (see local-stack.sh), curl, sed.
# SIMULATION ONLY: uses synthetic test card numbers.
set -euo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:8080}"
: "${GATEWAY_ADMIN_TOKEN:?set GATEWAY_ADMIN_TOKEN}"
RECEIVER_URL="${RECEIVER_URL:-http://127.0.0.1:9090/hooks}"
WORKDIR="${WORKDIR:-$(mktemp -d)}"
RECEIVER_BIN="${RECEIVER_BIN:-}"   # optional: path to webhook-receiver to launch
mkdir -p "$WORKDIR"

# field NAME: extract the first "NAME":"value" string from compact JSON on stdin.
field() { { grep -o "\"$1\":\"[^\"]*\"" || true; } | head -n 1 | sed 's/^"[^"]*":"\(.*\)"$/\1/'; }
# num NAME: extract the first "NAME":123 number.
num() { { grep -o "\"$1\":-\{0,1\}[0-9]*" || true; } | head -n 1 | sed 's/.*://'; }
# count PATTERN: number of occurrences of PATTERN on stdin.
count() { { grep -o "$1" || true; } | wc -l | tr -d ' '; }

admin() { curl -sS -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" -H "Content-Type: application/json" "$@"; }
merchant() { curl -sS -H "Authorization: Bearer $SK" -H "Content-Type: application/json" "$@"; }
step() { printf '\n== %s\n' "$*"; }

step "health"
curl -sS "$GATEWAY_URL/healthz"; echo

step "onboard merchant"
EMAIL="walkthrough-$(date +%s)@example.test"
M=$(admin -X POST "$GATEWAY_URL/admin/v1/merchants" \
  -d "{\"name\":\"Walkthrough Shop\",\"email\":\"$EMAIL\",\"country\":\"US\",\"default_currency\":\"USD\",\"activate\":true}")
MID=$(echo "$M" | field id); echo "merchant: $MID"

step "issue API key"
K=$(admin -X POST "$GATEWAY_URL/admin/v1/merchants/$MID/api_keys" -d '{"label":"walkthrough"}')
SK=$(echo "$K" | field secret); echo "key prefix: $(echo "$K" | field prefix)"

step "register webhook endpoint -> $RECEIVER_URL"
EP=$(merchant -X POST "$GATEWAY_URL/v1/webhook_endpoints" -d "{\"url\":\"$RECEIVER_URL\",\"events\":[\"*\"]}")
EPID=$(echo "$EP" | field id)
echo "$EP" | field secret > "$WORKDIR/webhook-secret.txt"
echo "endpoint: $EPID (secret saved to $WORKDIR/webhook-secret.txt)"
if [ -n "$RECEIVER_BIN" ]; then
  WEBHOOK_SECRET="$(cat "$WORKDIR/webhook-secret.txt")" "$RECEIVER_BIN" \
    -addr "${RECEIVER_ADDR:-127.0.0.1:9090}" -out "$WORKDIR/webhooks.ndjson" > "$WORKDIR/receiver.log" 2>&1 &
  echo "$!" > "$WORKDIR/receiver.pid"
  sleep 1
fi

step "create + authorize payment (idempotent)"
PI=$(merchant -X POST "$GATEWAY_URL/v1/payment_intents" -H "Idempotency-Key: walkthrough-$MID" \
  -d '{"amount":10000,"currency":"USD","description":"walkthrough order","confirm":true,
       "payment_method":{"card":{"number":"4242424242424242","exp_month":12,"exp_year":2030,"cvc":"123"}}}')
PID=$(echo "$PI" | field id)
echo "payment: $PID status=$(echo "$PI" | field status) card=$(echo "$PI" | field masked_number)"
REPLAY=$(merchant -X POST "$GATEWAY_URL/v1/payment_intents" -H "Idempotency-Key: walkthrough-$MID" \
  -d '{"amount":10000,"currency":"USD","description":"walkthrough order","confirm":true,
       "payment_method":{"card":{"number":"4242424242424242","exp_month":12,"exp_year":2030,"cvc":"123"}}}')
echo "idempotent replay returned the same payment: $([ "$(echo "$REPLAY" | field id)" = "$PID" ] && echo yes || echo NO)"

step "partial capture 80.00"
C=$(merchant -X POST "$GATEWAY_URL/v1/payment_intents/$PID/capture" -d '{"amount":8000}')
echo "status=$(echo "$C" | field status) captured=$(echo "$C" | num amount_captured)"

step "refund 30.00"
R=$(merchant -X POST "$GATEWAY_URL/v1/payment_intents/$PID/refunds" -d '{"amount":3000,"reason":"requested_by_customer"}')
echo "refund=$(echo "$R" | field id) status=$(echo "$R" | field status)"

step "declined card example"
D=$(merchant -X POST "$GATEWAY_URL/v1/payment_intents" \
  -d '{"amount":500,"currency":"USD","confirm":true,
       "payment_method":{"card":{"number":"4000000000009995","exp_month":12,"exp_year":2030,"cvc":"123"}}}')
echo "status=$(echo "$D" | field status) decline_code=$(echo "$D" | field decline_code)"

TODAY=$(date -u +%F)
step "run settlement for $TODAY (allow_open_day)"
S=$(admin -X POST "$GATEWAY_URL/admin/v1/settlements/run" -d "{\"date\":\"$TODAY\",\"merchant_id\":\"$MID\",\"allow_open_day\":true}")
echo "$S"
BATCH=$(echo "$S" | sed -n 's/.*"batches":\[{"id":"\([^"]*\)".*/\1/p')
if [ -n "$BATCH" ]; then
  step "settlement report (CSV)"
  merchant "$GATEWAY_URL/v1/settlements/$BATCH/report?format=csv"
fi

step "reconcile $TODAY against the simulated acquirer file"
admin -X POST "$GATEWAY_URL/admin/v1/reconciliations" -d "{\"date\":\"$TODAY\"}"; echo

step "wait for webhook deliveries"
for _ in $(seq 1 20); do
  PENDING=$(merchant "$GATEWAY_URL/v1/webhook_endpoints/$EPID/deliveries?status=pending" | count '"id":"whd_')
  [ "$PENDING" = "0" ] && break
  sleep 0.5
done
OK=$(merchant "$GATEWAY_URL/v1/webhook_endpoints/$EPID/deliveries?status=succeeded&limit=100" | count '"id":"whd_')
PENDING=$(merchant "$GATEWAY_URL/v1/webhook_endpoints/$EPID/deliveries?status=pending&limit=100" | count '"id":"whd_')
echo "deliveries succeeded=$OK still_pending=$PENDING"
if [ -f "$WORKDIR/webhooks.ndjson" ]; then
  echo "receiver stored $(wc -l < "$WORKDIR/webhooks.ndjson" | tr -d ' ') verified events:"
  sed -n 's/.*"type":"\([^"]*\)".*/  \1/p' "$WORKDIR/webhooks.ndjson"
fi

step "audit chain + trial balance"
admin "$GATEWAY_URL/admin/v1/audit_logs/verify"; echo
admin "$GATEWAY_URL/admin/v1/ledger/trial_balance"; echo

if [ -f "$WORKDIR/receiver.pid" ]; then
  kill "$(cat "$WORKDIR/receiver.pid")" 2>/dev/null || true
fi
echo; echo "walkthrough complete (artifacts in $WORKDIR)"
