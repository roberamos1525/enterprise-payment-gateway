# enterprise-payment-gateway

> **SIMULATION ONLY — synthetic data — NOT PCI certified.**
> This project is a learning/portfolio implementation of a card payment gateway.
> It never connects to a real card network, acquirer or bank. Use it **only**
> with the synthetic test card numbers listed below (or Luhn-valid numbers you
> generate in reserved test ranges). Do not put real cardholder data into it.

A simulated payment gateway written in Go: merchant onboarding with hashed API
keys, a strict payment-intent state machine (authorize → capture → void →
refund), a simulated acquirer/card network with deterministic outcomes, AES-GCM
PAN tokenization, idempotency keys, a rules-based fraud engine, a transactional
webhook outbox with signed deliveries and retries, daily settlement with fees
and payouts, reconciliation against an acquirer settlement file, a
double-entry ledger and a hash-chained, append-only audit log. Persistence is
pure-Go SQLite (`modernc.org/sqlite`), so everything builds without a C
compiler.

## Contents

- [Implemented functionality](#implemented-functionality)
- [Technology stack](#technology-stack)
- [Architecture at a glance](#architecture-at-a-glance)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [API usage](#api-usage)
- [Test cards](#test-cards)
- [Testing](#testing)
- [Deployment](#deployment)
- [Limitations](#limitations)
- [Roadmap](#roadmap)
- [AI-assisted development](#ai-assisted-development)

Further reading: [GETTING_STARTED.md](GETTING_STARTED.md),
[API.md](API.md), [ARCHITECTURE.md](ARCHITECTURE.md),
[SECURITY.md](SECURITY.md), [docs/](docs/) (payment lifecycle, data model,
operations runbook, testing strategy).

## Implemented functionality

**Merchant management**
- Onboarding (pending or immediately active), profile updates, lifecycle
  `pending → active ⇄ suspended → closed` with validated transitions; closing
  revokes all keys.
- Versioned fee schedules (percentage in basis points + fixed fee per capture,
  fixed refund fee, optional return of the percentage fee on refunds) with
  `effective_from` dating — settlement uses the schedule in force that day.
- API keys `sk_test_<8-char prefix>_<32-char secret>`: only a SHA-256 hash is
  stored, the prefix identifies the key, constant-time comparison, optional
  expiry, rotation with a configurable grace period (old key keeps working
  until it expires), revocation, last-used tracking.
- Webhook endpoints with event subscriptions, per-endpoint signing secrets
  sealed with AES-GCM, secret rotation (old and new secret both sign during a
  grace window), enable/disable, ping.

**Payment lifecycle** — explicit state machine (`internal/payment/statemachine.go`):
`created → authorized → captured → partially_refunded/refunded`, plus
`requires_action` (3-D Secure), `requires_review` (fraud review), `declined`
(retryable with another card), `blocked`, `voided`, `canceled`. Full and
partial capture (remainder released), multiple partial refunds, void,
cancel, automatic capture, authorization expiry job. Every transition is
validated, recorded in a history table, audited and emitted as an event.
Optimistic locking plus a per-payment in-process lock prevent double captures.

**Simulated acquirer / card network**
- Luhn validation, BIN-range brand detection (Visa, Mastercard incl. 2-series,
  Amex, Discover, JCB, Diners, UnionPay, Maestro), brand-specific CVC length,
  expiry checks.
- Deterministic outcomes per test PAN: approve, `do_not_honor`,
  `insufficient_funds`, `lost_card`, `stolen_card`, `expired_card`,
  `incorrect_cvc`, `processing_error`, 3DS-required (then approve or decline),
  slow network, refund failure.
- Configurable latency (base + jitter + slow-card latency) with a timeout that
  surfaces as a retryable `503`.
- The acquirer keeps **its own** transaction books and produces a daily
  settlement CSV, optionally with deterministic injected discrepancies.

**PAN vault** — cards are tokenized (`tok_…`) and the PAN is sealed with
AES-256-GCM (key ring from the environment, primary + older keys, AAD binds
ciphertext to token and merchant). Keyed HMAC fingerprints identify a card
across tokens. Re-wrap endpoint migrates ciphertexts to a new primary key.
Raw PANs never appear in responses, logs, the audit log, events or any
database column other than the ciphertext; a log handler masks any PAN-like
digit sequence as defense in depth. CVCs are validated and discarded.

**Idempotency** — `Idempotency-Key` on every mutating merchant endpoint; the
request fingerprint (method, path, body) is persisted with the response; exact
retries are replayed (`Idempotent-Replayed: true`), payload changes are
rejected (`422 idempotency_key_reused`), concurrent duplicates get `409`, 5xx
responses release the key, stale locks are taken over, expired keys are purged.

**Webhooks** — transactional outbox (events and deliveries are written in the
same DB transaction as the state change), `Gateway-Signature: t=<unix>,v1=<hmac>`
HMAC-SHA256 signatures over `timestamp.body`, exponential backoff with jitter,
max attempts, leased claiming, per-attempt delivery log (status, latency,
error, response snippet), replay of any event, SSRF protection (HTTPS only and
no private/loopback/link-local targets by default, enforced again at dial time;
redirects are not followed).

**Fraud engine** — rules with scores and hard blocks: BIN blocklist (persisted,
admin-managed, seeded from config), velocity per card fingerprint / client IP /
merchant (sliding windows), amount thresholds (per-currency overrides),
billing-country vs card-country mismatch, IP-geolocation vs card-country
mismatch (synthetic GeoIP on documentation ranges), high-risk countries.
Total score maps to `allow`, `review` (manual approve/reject by an operator)
or `block`.

**Settlement & payouts** — daily batch per merchant and currency: unsettled
captures and refunds of the UTC day, per-item fees, gross/refunded/fees/net,
ledger postings, payout scheduled `payout_delay_days` later (or carried as a
negative balance when net ≤ 0), payout job marks payouts paid. Reports as JSON
or CSV, daily summary. Re-runs are safe (items settle exactly once); optional
automatic run for the previous day.

**Reconciliation** — the internal ledger's capture/refund journals for a day
are matched against the acquirer settlement file (simulator-generated or
uploaded CSV): `missing_at_acquirer`, `missing_internally`, `amount_mismatch`,
`currency_mismatch`, `type_mismatch`, `duplicate_at_acquirer`, plus per-currency
net totals. Uploaded files are validated and rejected if they contain a PAN.

**Ledger** — double-entry journals (capture, refund, settlement fee, payout,
payout paid) with balance validation, idempotent posting per business event,
per-merchant balances and a trial balance endpoint.

**Audit log** — append-only (SQLite triggers reject UPDATE/DELETE), every
entry records actor type/id, action, resource, merchant, request id, client IP
and metadata, and is SHA-256 hash-chained to its predecessor; a verify
endpoint recomputes the chain.

**Platform** — merchant API-key auth and admin bearer tokens (hashed, constant
time), per-key token-bucket rate limiting (`X-RateLimit-*`, `Retry-After`),
strict JSON decoding (unknown fields rejected, size limits), consistent error
envelope, cursor pagination, request ids, security headers, structured JSON
logs with PAN redaction, Prometheus-format metrics (`/metrics`), health and
readiness probes, background jobs, graceful shutdown, `gatewayctl` operator
CLI and a signature-verifying `webhook-receiver`.

## Technology stack

| Concern | Choice |
|---|---|
| Language | Go 1.26 (standard library `net/http` routing, `log/slog`) |
| Persistence | SQLite via `modernc.org/sqlite` (pure Go, no CGO), embedded SQL migrations, WAL |
| Crypto | `crypto/aes` + `crypto/cipher` (AES-256-GCM), `crypto/hmac`, `crypto/sha256` |
| Metrics | In-repo Prometheus text-format registry (`internal/metrics`) |
| Tests | `go test`, `net/http/httptest`, fake clock, real SQLite in temp dirs |

The only third-party dependency is the SQLite driver (and its transitive
modules); `go.sum` is committed.

## Architecture at a glance

```
cmd/gateway ─ cmd/gatewayctl ─ cmd/webhook-receiver
      │
internal/app (wiring, lifecycle) ── internal/jobs (expiry, purge, payouts, auto-settle)
      │
internal/httpapi (routes, auth, rate limit, idempotency, error envelope)
      │
 merchant · payment · settlement · reconcile · fraud · webhook (outbox + dispatcher)
      │            │
   acquirer (simulated network) · vault (AES-GCM tokens) · ledger · audit · idempotency
      │
internal/store (SQLite, migrations, Queries usable inside one transaction)
```

A state change, its history row, ledger postings, outbox event + deliveries
and the audit record commit in **one** SQLite transaction. See
[ARCHITECTURE.md](ARCHITECTURE.md).

## Quick start

```bash
# Build the three binaries
go build -o bin/ ./cmd/...

# Development secrets (never reuse these)
export GATEWAY_VAULT_KEYS="$(go run ./cmd/gatewayctl gen-key -id k1)"
export GATEWAY_FINGERPRINT_SECRET="dev-fingerprint-secret-change-me"
export GATEWAY_ADMIN_TOKENS="ops:dev-admin-token-0123456789abcdef"
export GATEWAY_WEBHOOK_ALLOW_HTTP=true GATEWAY_WEBHOOK_ALLOW_PRIVATE=true  # local receiver
./bin/gateway                      # listens on 127.0.0.1:8080
```

Or run the whole scripted demo (builds, starts gateway + receiver, walks
through onboarding → payment → settlement → reconciliation → webhooks, stops
everything):

```bash
bash examples/local-stack.sh
```

Step-by-step instructions: [GETTING_STARTED.md](GETTING_STARTED.md).

## Configuration

Defaults → optional JSON file (`-config` / `GATEWAY_CONFIG_FILE`, unknown keys
rejected) → `GATEWAY_*` environment variables. Secrets are read from the
environment only:

| Variable | Purpose |
|---|---|
| `GATEWAY_VAULT_KEYS` | `id:base64(32 bytes)[,…]`, first is primary (required) |
| `GATEWAY_FINGERPRINT_SECRET` | HMAC key for card fingerprints, ≥16 chars (required) |
| `GATEWAY_ADMIN_TOKENS` | `name:token[,…]`, tokens ≥24 chars (required) |
| `GATEWAY_ENV` | `prod` (default) or `dev` (dev may generate an *ephemeral* vault key) |

All other settings (listen address, timeouts, rate limits, fraud thresholds,
fees, payout delay, webhook policy/backoff, job intervals…) are documented in
[`.env.example`](.env.example) and
[`examples/config/gateway.example.json`](examples/config/gateway.example.json).
`gateway -check-config` validates a configuration without starting.

## API usage

```bash
ADMIN="Authorization: Bearer $ADMIN_TOKEN"; JSON="Content-Type: application/json"
curl -s -H "$ADMIN" -H "$JSON" -X POST localhost:8080/admin/v1/merchants \
  -d '{"name":"Shop","email":"shop@example.test","country":"US","default_currency":"USD","activate":true}'
curl -s -H "$ADMIN" -H "$JSON" -X POST localhost:8080/admin/v1/merchants/$MID/api_keys -d '{}'

SK="Authorization: Bearer sk_test_..."
curl -s -H "$SK" -H "$JSON" -H "Idempotency-Key: order-1" -X POST localhost:8080/v1/payment_intents \
  -d '{"amount":10000,"currency":"USD","confirm":true,
       "payment_method":{"card":{"number":"4242424242424242","exp_month":12,"exp_year":2030,"cvc":"123"}}}'
curl -s -H "$SK" -H "$JSON" -X POST localhost:8080/v1/payment_intents/$PI/capture -d '{"amount":8000}'
curl -s -H "$SK" -H "$JSON" -X POST localhost:8080/v1/payment_intents/$PI/refunds -d '{"amount":3000}'
```

Money is always an integer amount in **minor units** plus an ISO 4217 currency
(`"amount":10000,"currency":"USD"` = 100.00 USD; JPY has no minor unit, KWD
has three). The full reference with every endpoint, status code and error code
is in [API.md](API.md).

## Test cards

All numbers are synthetic, publicly documented test numbers.
`GET /v1/test_cards` and `gatewayctl testcards` list them.

| Number | Outcome |
|---|---|
| 4242 4242 4242 4242, 5555 5555 5555 4444, 3782 822463 10005, 6011 1111 1111 1117 … | approved |
| 4000 0000 0000 0002 | declined `do_not_honor` |
| 4000 0000 0000 9995 | declined `insufficient_funds` |
| 4000 0000 0000 9987 / 9979 | declined `lost_card` / `stolen_card` |
| 4000 0000 0000 0069 / 0127 | declined `expired_card` / `incorrect_cvc` |
| 4000 0000 0000 0119 | `processing_error` (retryable) |
| 4000 0000 0000 3220 | 3-D Secure required, approved after success |
| 4000 0084 0000 1629 | 3-D Secure required, declined afterwards |
| 4000 0000 0000 0341 | extra simulated latency (timeout demo) |
| 4000 0000 0000 5126 | approved, refunds fail |
| 4100 0000 0000 0019 | BIN on the default fraud blocklist → `blocked` |
| 4000 0082 6000 0000 / 4000 0027 6000 0016 / 4000 0007 6000 0002 | issued in GB / DE / BR (country-mismatch demos) |

Any other Luhn-valid number of a supported brand is approved.
`gatewayctl gen-pan -prefix 400000 -n 5` generates more.

## Testing

```bash
go test ./... -count=1        # 141 tests in 29 packages, all passing
gofmt -l . && go vet ./...    # both clean
```

Unit tests live beside the code; `tests/integration` runs complete flows
against the real HTTP handler with an in-process server, a temporary SQLite
database, a fake clock and `httptest` webhook receivers that verify
signatures. The race detector (`-race`) was **not** run locally because CGO is
unavailable on the development machine; CI is configured to run it on Linux.
Details: [docs/testing.md](docs/testing.md).

## Deployment

A multi-stage [`Dockerfile`](Dockerfile) (static pure-Go binaries on Alpine,
non-root user, `/data` volume, health check) and a
[`compose.yaml`](compose.yaml) demo stack (gateway + webhook receiver) are
provided. **They were written carefully but not built or run locally — Docker
is not installed on the development machine.** The CI workflow in
`.github/workflows/ci.yml` (lint, test on Linux/Windows with `-race` on Linux,
scripted smoke test, image build) is a local file that has never been executed
on GitHub. Operational guidance: [docs/operations.md](docs/operations.md).

## Limitations

- **Not a real gateway and not PCI DSS compliant.** No real network
  connectivity, no HSM/KMS, no key ceremony, no network segmentation, no
  tokenization service isolation, no penetration testing.
- Single-node design: SQLite with one connection; fraud velocity counters,
  rate-limit buckets and per-payment locks are in-process memory (lost on
  restart, not shared between instances).
- One capture per authorization (partial capture releases the remainder);
  no incremental authorizations, multi-capture, disputes/chargebacks,
  currency conversion, or payment methods other than cards.
- 3-D Secure is a simulated challenge (`POST …/3ds` with `success|failure`),
  not EMV 3DS.
- Webhook ordering is not guaranteed (deliveries run concurrently); receivers
  should use the event `created_at`/resource state.
- Fee schedules are per merchant, not per currency; fixed fees are in the
  minor units of the transaction currency.
- Settlement days are UTC calendar days; payouts are only marked paid (no bank
  file generation).
- Reconciliation totals per currency are returned when a run is created but
  are not persisted (discrepancies are).
- GeoIP is a synthetic table of documentation ranges.
- Docker image/compose and the CI workflow were not executed; `-race` was not
  run locally.

## Roadmap

- Pluggable storage (PostgreSQL adapter) and shared rate-limit/velocity state
  (e.g. Redis) for multi-instance deployments.
- Disputes/chargebacks and their ledger/settlement impact.
- Multi-capture and incremental authorization.
- Per-currency fee schedules and FX-aware settlement.
- KMS-backed envelope encryption for the vault.
- Webhook ordering keys per resource and a dead-letter view in the API.
- OpenAPI specification generated from the route table.

## AI-assisted development

This project was developed with AI assistance (Claude by Anthropic). The code,
tests and documentation were generated iteratively with the assistant and
reviewed/verified by building the binaries, running `gofmt`, `go vet` and the
full test suite (`go test ./... -count=1`, run repeatedly), and by smoke-testing
the running service end to end with `curl` (see
[docs/testing.md](docs/testing.md)). Claims in this documentation are limited
to what was actually executed; unverified items (Docker, CI, race detector)
are marked as such.

## License

MIT — see [LICENSE](LICENSE).
