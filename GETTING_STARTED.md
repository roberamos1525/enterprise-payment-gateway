# Getting started

> Simulation only. Use the synthetic test cards from `GET /v1/test_cards`.

## Prerequisites

- Go 1.26 or newer (no C compiler needed — SQLite is pure Go)
- `curl` for the examples; `bash` for the scripts in `examples/` (Git Bash
  works on Windows)

## 1. Build

```bash
go build -o bin/ ./cmd/...        # gateway, gatewayctl, webhook-receiver
go test ./... -count=1            # optional: run the test suite
```

## 2. Configure secrets

The gateway refuses to start without vault keys, a fingerprint secret and
admin tokens.

```bash
export GATEWAY_VAULT_KEYS="$(./bin/gatewayctl gen-key -id k1)"   # k1:<base64 32 bytes>
export GATEWAY_FINGERPRINT_SECRET="$(openssl rand -hex 24)"     # any >= 16 chars
export GATEWAY_ADMIN_TOKENS="ops:$(openssl rand -hex 24)"
export GATEWAY_ADMIN_TOKEN="${GATEWAY_ADMIN_TOKENS#ops:}"       # for gatewayctl / curl
```

For a receiver on your own machine also allow plain HTTP and private
addresses (never do this in production):

```bash
export GATEWAY_WEBHOOK_ALLOW_HTTP=true GATEWAY_WEBHOOK_ALLOW_PRIVATE=true
```

Alternatively copy `.env.example` to `.env`, fill it in and export it, or use
a JSON file: `./bin/gateway -config examples/config/gateway.example.json`.
`./bin/gateway -check-config` validates without starting.

## 3. Run

```bash
./bin/gateway
# {"level":"INFO","msg":"gateway started","addr":"127.0.0.1:8080",...}
curl -s localhost:8080/healthz
```

Data lives in `gateway.db` (+ `-wal`/`-shm`) in the working directory unless
`GATEWAY_DB_PATH` says otherwise. Stop with Ctrl+C (graceful shutdown).

## 4. Walk through a payment

```bash
B=http://127.0.0.1:8080; JSON="Content-Type: application/json"; A="Authorization: Bearer $GATEWAY_ADMIN_TOKEN"

# Onboard + key (or: ./bin/gatewayctl merchant-create ... / key-create ...)
curl -s -H "$A" -H "$JSON" -X POST $B/admin/v1/merchants \
  -d '{"name":"Demo","email":"demo@example.test","country":"US","default_currency":"USD","activate":true}'
curl -s -H "$A" -H "$JSON" -X POST $B/admin/v1/merchants/<mer_id>/api_keys -d '{"label":"demo"}'
SK="Authorization: Bearer <sk_test_... secret>"

# Authorize 100.00 USD with a synthetic card
curl -s -H "$SK" -H "$JSON" -H "Idempotency-Key: demo-1" -X POST $B/v1/payment_intents \
  -d '{"amount":10000,"currency":"USD","confirm":true,"payment_method":{"card":{"number":"4242424242424242","exp_month":12,"exp_year":2030,"cvc":"123"}}}'

# Capture 80.00, refund 30.00
curl -s -H "$SK" -H "$JSON" -X POST $B/v1/payment_intents/<pi_id>/capture -d '{"amount":8000}'
curl -s -H "$SK" -H "$JSON" -X POST $B/v1/payment_intents/<pi_id>/refunds -d '{"amount":3000}'

# Settle today early, then reconcile against the simulated acquirer
curl -s -H "$A" -H "$JSON" -X POST $B/admin/v1/settlements/run -d "{\"date\":\"$(date -u +%F)\",\"allow_open_day\":true}"
curl -s -H "$A" -H "$JSON" -X POST $B/admin/v1/reconciliations -d "{\"date\":\"$(date -u +%F)\"}"
```

## 5. Receive webhooks

```bash
# Register an endpoint; note the returned "secret"
curl -s -H "$SK" -H "$JSON" -X POST $B/v1/webhook_endpoints \
  -d '{"url":"http://127.0.0.1:9090/hooks","events":["*"]}'
WEBHOOK_SECRET=whsec_... ./bin/webhook-receiver -addr 127.0.0.1:9090 -out webhooks.ndjson
```

Every verified event is appended to `webhooks.ndjson`. Delivery attempts are
visible at `GET /v1/webhook_endpoints/<we_id>/deliveries` and
`GET /v1/webhook_deliveries/<whd_id>`.

## 6. Scripted demo

```bash
bash examples/local-stack.sh      # build, start gateway + receiver, run the walkthrough, stop
```

`examples/curl-walkthrough.sh` can also be pointed at an already running
gateway (`GATEWAY_URL`, `GATEWAY_ADMIN_TOKEN`). `examples/go-client` shows the
same flow from Go (with a 3-D Secure card):

```bash
GATEWAY_URL=$B GATEWAY_API_KEY=sk_test_... go run ./examples/go-client
```

## Useful CLI commands

```bash
./bin/gatewayctl testcards                     # synthetic cards and scenarios
./bin/gatewayctl gen-pan -prefix 400000 -n 3   # more Luhn-valid synthetic PANs
./bin/gatewayctl audit-verify                  # check the audit hash chain
./bin/gatewayctl job expire_authorizations     # run a background job now
./bin/gatewayctl acquirer-file -date 2026-03-02 > acq.csv
./bin/gatewayctl reconcile -date 2026-03-02 -file acq.csv
```

## Troubleshooting

| Symptom | Cause |
|---|---|
| `config: GATEWAY_VAULT_KEYS is required` | Set the three secrets (step 2). |
| `422 … must use https` / `must not target a private or reserved address` when registering a webhook | Enable the two `GATEWAY_WEBHOOK_ALLOW_*` flags for local receivers. |
| `400 settlement date … has not ended yet` | Use `"allow_open_day": true` or settle a past day. |
| Payment stays `requires_review` | Large amount / risk signals; approve via `POST /admin/v1/payment_intents/{id}/review`. |
| `415 Content-Type must be application/json` | `curl -d` defaults to form encoding; add `-H "Content-Type: application/json"`. |
