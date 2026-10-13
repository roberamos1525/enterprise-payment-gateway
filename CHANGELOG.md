# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [0.1.0] - 2026-10-12

First release of the simulated gateway (synthetic data only, not PCI certified).

### Added
- Merchant onboarding, lifecycle statuses, versioned fee schedules.
- Hashed, prefix-identified API keys with expiry, rotation grace periods and
  revocation; admin bearer tokens.
- Payment-intent state machine: authorize, 3-D Secure challenge, manual
  review, full/partial capture, void, cancel, multiple partial refunds,
  automatic capture, authorization expiry.
- Simulated acquirer with deterministic test-card outcomes, latency
  simulation, its own transaction books and a daily settlement file with
  optional injected discrepancies.
- AES-256-GCM PAN vault with key ring, HMAC fingerprints and re-wrap; PAN
  redaction in logs and audit metadata.
- Idempotency keys with request fingerprinting and response replay.
- Fraud engine: BIN blocklist, card/IP/merchant velocity, amount thresholds,
  billing/IP country mismatch, high-risk countries; allow/review/block.
- Webhooks: transactional outbox, HMAC-SHA256 timestamped signatures, secret
  rotation, SSRF-guarded dispatcher with exponential backoff, delivery logs,
  replay and ping.
- Daily settlement per merchant and currency with fees, payouts, JSON/CSV
  reports and summaries; payout job; optional automatic settlement.
- Reconciliation of ledger journals against the acquirer settlement file
  (simulator or uploaded CSV) with discrepancy reports.
- Double-entry ledger, balances and trial balance.
- Append-only, hash-chained audit log with verification.
- Token-bucket rate limiting, strict JSON validation, error envelope, cursor
  pagination, request ids, security headers.
- Structured logging, Prometheus-format metrics, health/readiness probes,
  background job runner, graceful shutdown.
- `gatewayctl` operator CLI and `webhook-receiver` sample endpoint.
- SQLite persistence via `modernc.org/sqlite` with embedded migrations.
- Unit and integration test suites; examples; Dockerfile, compose file and
  CI workflow (not executed locally).
