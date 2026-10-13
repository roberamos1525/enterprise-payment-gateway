# Architecture

> Simulation only — no real payment network is ever contacted.

## Goals and constraints

- Model the moving parts of a card gateway faithfully enough to exercise real
  engineering problems: state machines, idempotency, exactly-once side
  effects, outbox-based notifications, double-entry money movement, settlement
  and reconciliation, key management for card data.
- Build and test on any machine with only a Go toolchain: pure-Go SQLite, no
  external services, no CGO.
- Keep sensitive data handling explicit: one place (`vault`) can see a PAN,
  and only the simulated acquirer asks it to.

## Package layout

```
cmd/
  gateway/            HTTP service (config → app.New → Serve, graceful shutdown)
  gatewayctl/         operator CLI (thin main over internal/cli)
  webhook-receiver/   local signature-verifying webhook sink
internal/
  app/          wiring of every component, background loops, HTTP server lifecycle
  httpapi/      routes, auth + rate-limit + idempotency middleware, handlers, error envelope
  merchant/     onboarding, lifecycle, fee schedules, API keys, webhook endpoints
  payment/      payment-intent state machine and lifecycle service
  acquirer/     simulated network: outcomes, latency, own books, settlement file
  vault/        AES-GCM key ring, tokenization, fingerprints, re-wrap
  fraud/        rules engine, velocity windows, synthetic GeoIP, BIN blocklist
  webhook/      outbox, signer/verifier, SSRF-guarded dispatcher, replay, ping
  settlement/   daily batches, fees, payouts, reports
  reconcile/    ledger vs acquirer file matching
  ledger/       double-entry postings, balances, trial balance
  audit/        append-only hash-chained audit log
  idempotency/  request fingerprinting, claim/replay of responses
  store/        SQLite access (Queries), embedded migrations
  model/        persistent entity types (no behaviour)
  view/         public JSON shapes shared by API responses and webhook payloads
  card/ money/  PAN utilities and test-card catalog; minor-unit money, fees, rounding
  auth/ ratelimit/ metrics/ logging/ config/ jobs/ reqctx/ clock/ ids/ apperr/
tests/
  testenv/      in-process gateway builder for tests
  integration/  end-to-end flows over HTTP
```

Dependency direction is strictly downward: `httpapi → services → (acquirer,
vault, ledger, audit, webhook outbox) → store → model`. Services never import
`httpapi`, and `model` has no internal imports.

## Request path

```
client ─▶ withRequestContext (request id, client IP, security headers, body limit)
       ─▶ withAccessLog (metrics, access log, panic → 500 envelope)
       ─▶ jsonFallback (404/405 as JSON)
       ─▶ ServeMux route ─▶ merchantOnly | adminOnly | anonymous   (auth + token bucket)
                          ─▶ idempotent (merchant mutations)
                          ─▶ handler ─▶ service
```

`reqctx` carries the request id, client IP and the authenticated actor; the
audit logger reads them, so services never thread them manually.

## Unit of work

`store.DB` owns a single SQLite connection (WAL, `busy_timeout`,
`foreign_keys`, `_txlock=immediate`). `DB.Tx(ctx, fn)` hands `fn` a
`*store.Queries` bound to the transaction; every repository method lives on
`Queries`, so a service can compose writes across aggregates:

```
payment.capture:
  acquirer.Capture(...)                        // outside the DB tx (network call)
  DB.Tx:
    UpdatePayment (optimistic version check)
    InsertHistory
    ledger.RecordCapture  (journal + entries, unique (kind, ref))
    outbox.Publish        (event + one delivery per subscribed endpoint)
    audit.Record          (hash-chained entry)
```

Either all of it commits or none of it does. Webhooks therefore can neither be
lost for a committed change nor sent for a rolled-back one.

Concurrency control for a payment has two layers: a keyed in-process mutex
serialises operations on the same payment id (so two concurrent captures never
both reach the acquirer), and `UpdatePayment` uses an optimistic `version`
column as a safety net (`409 conflict` if it ever trips).

Calls to the simulated acquirer happen outside the gateway transaction, as a
real network call would. The acquirer writes its own records in its own short
transaction — the two sets of books are independent, which is what makes
reconciliation meaningful.

## Payment state machine

Defined as data in `internal/payment/statemachine.go` (action → allowed source
states → allowed target states). `commit` validates every transition against
the table before writing, so a bug in service code cannot produce an illegal
state. See [docs/payment-lifecycle.md](docs/payment-lifecycle.md).

## Card data flow

```
POST card JSON ─▶ card.Validate (Luhn, brand, expiry, CVC length)
               ─▶ vault.Tokenize: Seal(PAN, AAD=token|merchant) with primary key
                                  Fingerprint = HMAC-SHA256(fp secret, PAN)
                                  store ciphertext + BIN/last4/brand/expiry/country
               ─▶ response/event/audit only ever see view.Card (masked)
authorize ─▶ acquirer.scenarioFor(vault card) ─▶ vault.Reveal (in memory only)
```

The fingerprint key is independent of the encryption keys, so fingerprints
stay stable across key rotation. `vault.Rewrap` re-encrypts old ciphertexts
under a new primary key in batches. A `slog` handler redacts any Luhn-valid
12–19 digit sequence in log messages and attributes; the audit logger does the
same for metadata; the settlement-file parser rejects files containing PANs.

## Webhooks

- **Outbox**: `events` rows are immutable; `webhook_deliveries` rows hold the
  per-endpoint delivery state (`pending/succeeded/failed`, attempts,
  `next_attempt_at`, `locked_until`).
- **Dispatcher**: polls every `poll_interval`, claims due deliveries in a
  transaction by setting a lease (`locked_until`), sends them concurrently
  (bounded), records each attempt in `webhook_attempts` and either completes
  the delivery or schedules the next attempt with exponential backoff.
  Expired leases make crashed attempts eligible again.
- **Signing**: `t=<unix>,v1=<hex HMAC-SHA256(secret, "t.body")>`; during
  secret rotation both secrets sign.
- **SSRF guard**: URL validation at registration and a `net.Dialer.Control`
  hook that rejects non-public IPs after DNS resolution; proxies from the
  environment and redirects are ignored.

## Fraud engine

`fraud.Engine` evaluates a list of `Rule`s (`bin_blocklist`, `card_velocity`,
`ip_velocity`, `merchant_velocity`, `amount_threshold`,
`billing_country_mismatch`, `ip_country_mismatch`, `high_risk_country`). Each
returns an optional signal with a score and an optional hard block. Score
≥ `block_score` or any hard block → `block`; ≥ `review_score` → `review`.
Velocity uses in-memory sliding-window logs keyed by card fingerprint, client
IP and merchant, swept periodically. Rules can be disabled by name.

## Money, fees and the ledger

`money` represents amounts as `int64` minor units with ISO 4217 exponents;
percentage fees use `math/big` with selectable rounding (half-even default),
so no floating point is ever involved. Ledger accounts:

| Account | Normal side | Moves on |
|---|---|---|
| `acquirer_receivable` | debit | +capture, −refund |
| `merchant_payable` | credit | +capture, −refund, −fees, −payout |
| `fee_revenue` | credit | +fees (−returned fees) |
| `payouts_pending` | credit | +payout scheduled, −payout paid |
| `cash` | debit | −payout paid |

Journals are unique per `(kind, ref)`, so a retried posting cannot double-book.

## Settlement and reconciliation

Settlement selects captures (`payment_intents.captured_at`) and succeeded
refunds of a UTC day that are not yet in `settlement_items` (primary key
`(kind, ref_id)` guarantees each item settles once), groups them per currency,
applies the fee schedule effective at the end of that day, inserts the batch +
items, posts fee and payout journals, schedules a payout and emits
`settlement.created` / `payout.scheduled`.

Reconciliation reads the day's `capture`/`refund` journals from the ledger
(keyed by acquirer reference) and matches them against the acquirer's file
rows by reference. The pure matching function `reconcile.Match` is tested in
isolation.

## Background jobs

`jobs.Runner` runs named jobs on intervals (never overlapping with
themselves) and exposes them for manual runs via
`POST /admin/v1/jobs/{name}/run`: `expire_authorizations`,
`purge_idempotency` (also sweeps velocity and rate-limit state),
`pay_payouts`, `settle_previous_day` (scheduled only when
`settlement.auto_run` is true) and `dispatch_webhooks` (manual; the dispatcher
has its own loop).

## Observability

- JSON logs via `log/slog`, UTC timestamps, PAN redaction, access log with
  route pattern, status, latency, client IP and actor.
- `/metrics` in Prometheus text format from a small in-repo registry
  (counters, histograms, scrape-time gauges backed by DB counts).
- `/healthz`, `/readyz`; request ids in every error body and audit entry.

## Key design decisions

| Decision | Rationale |
|---|---|
| SQLite, single connection | Pure Go, zero setup, serialised writers avoid `SQLITE_BUSY`; adequate for a simulator. |
| Transactional outbox instead of publishing after commit | No lost or phantom webhooks. |
| Declines as `200` business outcomes | Clients always get the full resource; only invalid requests are errors. |
| One capture per authorization | Matches the most common card flow and keeps the acquirer simulation exact. |
| In-repo metrics registry | Avoids a heavy dependency for a handful of metric types; output is standard Prometheus text. |
| Separate acquirer books | Reconciliation compares two genuinely independent records. |
