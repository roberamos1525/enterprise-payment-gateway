# Operations runbook

> For the simulator only. Nothing here makes it suitable for real payments.

## Starting and stopping

```bash
gateway [-config gateway.json]      # SIGINT/SIGTERM → graceful shutdown (shutdown_timeout)
gateway -check-config               # validate configuration and exit
gateway -version
```

On start the gateway opens/migrates the database, seeds the configured BIN
blocklist, starts the webhook dispatcher and periodic jobs, then listens.
Startup fails fast on invalid configuration (all problems are listed at once).

## Health and monitoring

| Signal | Where |
|---|---|
| Liveness | `GET /healthz` |
| Readiness (DB reachable) | `GET /readyz` |
| Metrics | `GET /metrics` (admin token) |
| Logs | JSON on stdout; `msg="http request"` access lines with route, status, duration, client IP, actor |

Useful metrics: `gateway_http_requests_total{status="5xx"}`,
`gateway_payment_operations_total`, `gateway_fraud_decisions_total{action}`,
`gateway_webhook_attempts_total{outcome="failed"}`,
`gateway_webhook_deliveries{status="pending"}` (backlog),
`gateway_rate_limited_total`, `gateway_reconciliation_runs_total{status="discrepancies"}`,
`gateway_job_runs_total{result="error"}`.

## Routine tasks

| Task | How |
|---|---|
| Settle a day | `POST /admin/v1/settlements/run {"date":"YYYY-MM-DD"}` or `gatewayctl settle -date …`; automatic when `settlement.auto_run=true` (after `auto_run_hour_utc`). Safe to re-run. |
| Pay due payouts | automatic every `payout_interval`; manual `POST /admin/v1/payouts/pay_due`. |
| Reconcile | `POST /admin/v1/reconciliations {"date":…}` (simulator file) or upload a CSV to `/admin/v1/reconciliations/upload?date=…`. |
| Expire stale authorizations | automatic every `authorization_expiry_interval`; manual job `expire_authorizations`. |
| Purge idempotency keys / sweep in-memory state | job `purge_idempotency`. |
| Verify audit integrity | `GET /admin/v1/audit_logs/verify` / `gatewayctl audit-verify`. |
| Check ledger | `GET /admin/v1/ledger/trial_balance` must report `balanced: true`. |

## Incident playbooks

**Webhook backlog growing** — check `gateway_webhook_attempts_total` outcomes
and `GET /v1/webhook_endpoints/{id}/deliveries?status=pending`; inspect
`/v1/webhook_deliveries/{id}` attempt logs (status codes, errors such as
`timeout`, `connection refused`, `refusing to dial non-public address`). After
the receiver is fixed, failed deliveries can be re-sent with
`POST /v1/events/{id}/replay`.

**Reconciliation shows discrepancies** — list them via
`GET /admin/v1/reconciliations/{id}`. `missing_at_acquirer` means the ledger
booked a capture/refund the acquirer did not settle; `missing_internally`
means the acquirer settled something without a journal (the acquirer
approved but the gateway transaction did not commit); amount/currency/type
mismatches and duplicates point at file problems. The simulator injects such
cases deterministically when `acquirer.discrepancy_rate > 0`.

**Audit verification fails** — the response gives `broken_at_seq`; the row at
that sequence (or its predecessor) was modified outside the application.
Preserve the database file for investigation; do not attempt to "fix" rows.

**Acquirer timeouts (503)** — clients can retry with the same
`Idempotency-Key`; the payment is unchanged. Tune
`payments.acquirer_timeout` and the simulated latency settings.

**Too many 429s** — raise `rate_limit.*` or investigate the client; failed
authentications are limited per client IP.

## Key rotation

**Vault data key**
1. Generate: `gatewayctl gen-key -id k2`.
2. Prepend to `GATEWAY_VAULT_KEYS` (`k2:…,k1:…`) and restart — new cards use
   `k2`, old ones remain readable with `k1`.
3. `POST /admin/v1/vault/rewrap` until `GET /admin/v1/vault/status` shows no
   cards under `k1`.
4. Remove `k1` from the configuration and restart.

Keep `GATEWAY_FINGERPRINT_SECRET` unchanged (fingerprints would change).

**Merchant API keys** — `POST /admin/v1/merchants/{id}/api_keys/{key_id}/rotate`
(or `/v1/api_keys/rotate` by the merchant) with a grace period; revoke with
`DELETE` when the merchant has switched.

**Webhook secrets** — `POST /v1/webhook_endpoints/{id}/rotate_secret`; both
secrets sign for `secret_rotation_grace`.

**Admin tokens** — add the new `name:token` to `GATEWAY_ADMIN_TOKENS`,
restart, switch clients, remove the old entry, restart.

## Backup and restore

The state is the SQLite database (`db_path` plus `-wal`/`-shm`). Take backups
with the gateway stopped (or with SQLite's online backup tooling) and keep the
vault keys and fingerprint secret separately — a database backup without the
keys cannot decrypt tokenized cards, which is intended.

## Container deployment (not verified locally)

`Dockerfile` builds static binaries into an Alpine image running as a
non-root user with `/data` as a volume; `compose.yaml` adds the sample webhook
receiver. Neither was built or run on the development machine (no Docker
available). Run behind a TLS-terminating proxy and set
`GATEWAY_TRUST_PROXY_HEADERS=true` only when that proxy sets
`X-Forwarded-For`.
