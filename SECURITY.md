# Security

## Scope and disclaimer

**This is a simulation. It is not PCI DSS compliant, has not been audited or
penetration-tested, and must never process real cardholder data.** It
demonstrates security techniques used by payment systems; it is not a
substitute for them in production. Use only the synthetic test card numbers
documented in the README.

## Reporting

This is a personal portfolio project. If you find a vulnerability in the
code, please send a private report to the repository owner rather than
opening a public issue, and include steps to reproduce.

## Implemented controls

**Card data**
- PANs are tokenized immediately: sealed with AES-256-GCM using a key ring
  supplied via `GATEWAY_VAULT_KEYS`; the additional authenticated data binds
  each ciphertext to its token and merchant, so ciphertexts cannot be swapped
  between rows.
- Only BIN (first 6), last 4, brand, expiry and issuing country are stored in
  clear; the CVC is validated and discarded.
- Card fingerprints are HMAC-SHA256 with a separate secret, independent of the
  encryption keys.
- Raw PANs never appear in API responses, events/webhooks, the audit log or
  logs. Defense in depth: a log handler masks every Luhn-valid 12–19 digit
  sequence (messages, attributes, errors); audit metadata is scrubbed the same
  way; uploaded settlement files containing PAN-like values are rejected.
  `tests/integration/flow_test.go` (`TestNoPANLeaks`) asserts this against
  responses, logs and the raw database/WAL files.
- Key rotation: add a new primary key, restart, call
  `POST /admin/v1/vault/rewrap`; old keys can be removed afterwards.

**Authentication and authorization**
- Merchant API keys carry 160 bits of entropy; only SHA-256 hashes are
  stored, lookup is by public prefix, comparison is constant-time, and all
  failure modes (unknown, wrong secret, expired, revoked, closed merchant)
  return the same `401`.
- Key rotation with bounded grace periods; immediate revocation; closing a
  merchant revokes every key.
- Admin tokens come from the environment, must be at least 24 characters, are
  held only as SHA-256 digests and are compared in constant time against all
  configured tokens.
- Strict tenant isolation: every merchant query is scoped by merchant id;
  other merchants' resources are reported as `404`.

**Abuse and input handling**
- Token-bucket rate limits per API key, per admin, and per client IP for
  unauthenticated or failed-auth requests (slows brute force).
- Request bodies are size-limited, must be `application/json`, are decoded
  strictly (unknown fields rejected) and validated field by field.
- `X-Forwarded-For` is ignored unless `GATEWAY_TRUST_PROXY_HEADERS=true`.
- Security headers on every response (`nosniff`, `DENY` framing, restrictive
  CSP, `no-store`, `no-referrer`).
- HTTP server read/write/header timeouts and header size limits.

**Webhooks**
- HMAC-SHA256 signatures over `timestamp.body`; receivers should enforce a
  timestamp tolerance to prevent replay.
- Signing secrets are stored sealed (AES-GCM) and only shown once.
- SSRF protection: HTTPS only, no credentials, no loopback/private/link-local/
  CGNAT/reserved targets — validated on registration and enforced after DNS
  resolution in the dialer; environment proxies and redirects are not used.

**Integrity and auditability**
- Append-only audit log enforced by SQLite triggers, each entry hash-chained
  to the previous one; `GET /admin/v1/audit_logs/verify` detects tampering.
- Double-entry ledger with balance validation and unique journals per
  business event; trial-balance endpoint.
- Idempotency keys prevent duplicate charges on retries.

## Safe defaults

- Listens on `127.0.0.1:8080` by default.
- `GATEWAY_ENV=prod` by default; only `dev` may generate an ephemeral vault
  key (with a loud warning).
- Webhook policy is strict by default; the relaxed flags exist only for local
  development.
- `/metrics` requires an admin token.

## Known gaps

- No HSM/KMS integration; keys live in process memory and environment
  variables.
- No TLS termination in the service (run behind a TLS-terminating proxy).
- Velocity and rate-limit state is per process.
- No automated dependency/vulnerability scanning was run locally.
- The race detector was not run locally (no CGO on the development machine).
