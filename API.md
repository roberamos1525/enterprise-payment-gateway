# API reference

> Simulation only. Use synthetic test card numbers exclusively.

Base URL: `http://127.0.0.1:8080` by default. All request and response bodies
are JSON (`Content-Type: application/json`) unless stated otherwise.

## Conventions

**Authentication**

| Surface | Header | Credential |
|---|---|---|
| Merchant API `/v1/*` | `Authorization: Bearer sk_test_<prefix>_<secret>` | merchant API key |
| Operator API `/admin/v1/*`, `/metrics` | `Authorization: Bearer <admin token>` | token from `GATEWAY_ADMIN_TOKENS` |
| `/healthz`, `/readyz`, `/v1/test_cards` | none | — |

Invalid, expired, revoked or closed-merchant keys all produce the same `401`.
Resources of other merchants are reported as `404`.

**Money** — `amount` fields are integers in minor units of `currency`
(ISO 4217, case-insensitive on input, upper-case on output). Supported:
AUD BHD BRL CAD CHF CLP CZK DKK EUR GBP HKD HUF INR ISK JOD JPY KRW KWD MXN NOK
NZD OMR PLN SEK SGD USD ZAR. Responses add an `amount_display` string.

**Idempotency** — every mutating merchant endpoint marked *(idempotent)*
accepts `Idempotency-Key: <1-255 printable ASCII>`. Keys are scoped per
merchant and kept 24h (configurable).

| Situation | Result |
|---|---|
| first request | executed, response stored |
| retry with same method + path + body | stored response replayed, header `Idempotent-Replayed: true` |
| same key, different payload | `422 idempotency_key_reused` |
| same key while the first request is still running | `409 idempotency_key_in_progress` |
| original request ended with 5xx | key released, retry executes again |

**Pagination** — list endpoints return
`{"object":"list","data":[…],"has_more":bool,"next_cursor":"…"}` newest
first. Query: `limit` (1–100, default 20) and `starting_after=<next_cursor>`.

**Rate limiting** — token bucket per API key (merchants), per admin token
name, and per client IP for unauthenticated/failed requests. Every response
carries `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`
(seconds); `429` adds `Retry-After`.

**Request ids** — `X-Request-Id` is echoed (or generated) and appears in error
bodies, logs and audit entries.

**Limits** — bodies above `max_body_bytes` (1 MiB default) → `413`; unknown
JSON fields → `400`; wrong `Content-Type` → `415`.

### Error envelope

```json
{
  "error": {
    "type": "validation_failed",
    "message": "request validation failed",
    "fields": [{"field": "amount", "message": "must be between 1 and 100000000"}],
    "decline_code": "expired_card",
    "request_id": "req_06gj..."
  }
}
```

| `type` | HTTP |
|---|---|
| `invalid_request` | 400 |
| `unauthenticated` | 401 |
| `card_declined` | 402 (only for tokens that are already expired) |
| `forbidden` | 403 (e.g. merchant not active) |
| `not_found` | 404 |
| `method_not_allowed` | 405 |
| `conflict`, `invalid_state_transition`, `idempotency_key_in_progress` | 409 |
| `payload_too_large` | 413 |
| `unsupported_media_type` | 415 |
| `validation_failed`, `idempotency_key_reused` | 422 |
| `rate_limited` | 429 |
| `internal_error` | 500 |
| `service_unavailable` (acquirer timeout; safe to retry) | 503 |

Card declines and fraud blocks during authorization are **business outcomes**,
not errors: the endpoint returns `200` with `status: "declined"` or
`"blocked"` and a `decline_code` on the payment intent.

## Payment intent object

```json
{
  "id": "pi_06gjv57g0w0mjt7xxpedyc6yww",
  "object": "payment_intent",
  "merchant_id": "mer_…",
  "amount": 10000, "currency": "USD", "amount_display": "100.00 USD",
  "status": "captured",
  "capture_method": "manual",
  "amount_authorized": 10000, "amount_captured": 8000, "amount_refunded": 3000,
  "amount_capturable": 0, "amount_refundable": 5000,
  "card": {"token": "tok_…", "brand": "visa", "bin": "424242", "last4": "4242",
           "masked_number": "424242******4242", "country": "US", "fingerprint": "fp_…"},
  "authorization_code": "S5AWC2",
  "acquirer_reference": "acq_…", "capture_reference": "acq_…",
  "decline_code": "", "decline_message": "",
  "risk": {"score": 0, "action": "allow", "reasons": []},
  "next_action": null,
  "billing_country": "US", "description": "", "metadata": {},
  "created_at": "…", "updated_at": "…", "authorized_at": "…", "captured_at": "…",
  "authorization_expires_at": "…"
}
```

Statuses: `created`, `requires_action`, `requires_review`, `authorized`,
`captured`, `partially_refunded`, `refunded`, `voided`, `canceled`,
`declined`, `blocked`. Allowed transitions are listed in
[docs/payment-lifecycle.md](docs/payment-lifecycle.md); anything else returns
`409 invalid_state_transition`.

## Merchant API (`/v1`)

### Account

| Method & path | Description |
|---|---|
| `GET /v1/merchant` | The authenticated merchant. |
| `GET /v1/balance` | Ledger balances per account/currency (`merchant_payable`, `acquirer_receivable`, `fee_revenue`, `payouts_pending`, `cash`). |
| `GET /v1/api_keys` | The merchant's keys (prefix, status, expiry, last use; never secrets). |
| `POST /v1/api_keys/rotate` *(idempotent)* | Rotate the key used for this request. Body `{"grace_period_seconds": 3600}` (default 24h, 0 = revoke now, max 30 days). `201` with the new key incl. `secret` (shown once). |

### Cards

`POST /v1/tokens` *(idempotent)* — tokenize a card.

```json
{"card": {"number": "4242424242424242", "exp_month": 12, "exp_year": 2030, "cvc": "123", "holder_name": "optional"}}
```

`201` → `{"token":"tok_…","brand":"visa","bin":"424242","last4":"4242","masked_number":"424242******4242","country":"US","fingerprint":"fp_…","exp_month":12,"exp_year":2030,"funding":"credit"}`.
Validation: digits only (spaces/dashes stripped), 12–19 digits, Luhn, supported
brand, month 1–12, not expired, CVC length 3 (4 for Amex). The CVC is never stored.

### Payment intents

`POST /v1/payment_intents` *(idempotent)* → `201`

| Field | Type | Notes |
|---|---|---|
| `amount` | int | required, 1 … `max_amount_minor` |
| `currency` | string | required |
| `capture_method` | `manual` (default) \| `automatic` | automatic captures right after authorization |
| `description` | string | ≤ 500 chars |
| `metadata` | object | ≤ 20 string keys (≤40 chars), values ≤ 500 chars |
| `billing_country` | string | ISO 3166-1 alpha-2, used by fraud rules |
| `confirm` | bool | authorize immediately using `payment_method` |
| `payment_method` | object | `{"card":{…}}` or `{"token":"tok_…"}` (+ optional `billing_country`) |

| Method & path | Description |
|---|---|
| `GET /v1/payment_intents` | List; filters `status`, `created_after`, `created_before` (RFC 3339). |
| `GET /v1/payment_intents/{id}` | Retrieve. |
| `GET /v1/payment_intents/{id}/history` | State transitions (action, from, to, amount, detail, time). |
| `POST /v1/payment_intents/{id}/authorize` *(idempotent)* | Body: `{"card":{…}}` or `{"token":"tok_…"}`, optional `billing_country`. Allowed from `created` or `declined`. Runs fraud screening, then the simulated network. Result status: `authorized`, `requires_action`, `requires_review`, `declined`, `blocked` (or `captured` for automatic capture). |
| `POST /v1/payment_intents/{id}/3ds` *(idempotent)* | `{"challenge_id":"3ds_…","outcome":"success"\|"failure"}` from `next_action`. |
| `POST /v1/payment_intents/{id}/capture` *(idempotent)* | Optional `{"amount": n}` ≤ authorized (default full). One capture per payment; the remainder is released. |
| `POST /v1/payment_intents/{id}/void` *(idempotent)* | Release an authorization. Optional `{"reason": "requested_by_customer"\|"duplicate"\|"fraudulent"\|"abandoned"}`. |
| `POST /v1/payment_intents/{id}/cancel` *(idempotent)* | Cancel before authorization (from `created`, `requires_action`, `requires_review`, `declined`). Same reasons. |

### Refunds

`POST /v1/payment_intents/{id}/refunds` *(idempotent)* — body optional:
`{"amount": n, "reason": "requested_by_customer"|"duplicate"|"fraudulent"}`;
default refunds everything still refundable. Multiple partial refunds are
allowed. `201` returns the refund plus the updated `payment_intent`:

```json
{"id":"re_…","object":"refund","payment_intent_id":"pi_…","amount":3000,"currency":"USD",
 "status":"succeeded","reason":"requested_by_customer","acquirer_reference":"acq_…",
 "payment_intent":{…}}
```

If the acquirer rejects the refund the refund is still created with
`status: "failed"` and `failure_code: "refund_failed"`; the payment is unchanged.

| Method & path | Description |
|---|---|
| `GET /v1/payment_intents/{id}/refunds` | Refunds of a payment. |
| `GET /v1/refunds/{id}` | Retrieve a refund. |

### Webhook endpoints, events and deliveries

| Method & path | Description |
|---|---|
| `POST /v1/webhook_endpoints` *(idempotent)* | `{"url":"https://…","events":["payment_intent.captured"] or ["*"],"description":"…","enabled":true}` → `201` incl. `secret` (`whsec_…`, shown once). Max 10 per merchant. |
| `GET /v1/webhook_endpoints` | List. |
| `GET /v1/webhook_endpoints/{id}` | Retrieve (no secret). |
| `PATCH /v1/webhook_endpoints/{id}` | Change `url`, `events`, `description`, `enabled`. |
| `POST /v1/webhook_endpoints/{id}/rotate_secret` | New secret (returned once); the previous one keeps signing for `secret_rotation_grace`. |
| `POST /v1/webhook_endpoints/{id}/ping` | Queue a `webhook_endpoint.ping` delivery to this endpoint. `202`. |
| `GET /v1/webhook_endpoints/{id}/deliveries` | Deliveries, filter `status=pending|succeeded|failed`. |
| `GET /v1/webhook_deliveries/{id}` | Delivery with its `attempt_log` (attempt, status code, duration, error, response snippet). |
| `GET /v1/events` | Events; filters `type`, `resource_id`. |
| `GET /v1/events/{id}` | Retrieve an event. |
| `POST /v1/events/{id}/replay` | `{"endpoint_id":"we_…"}` optional; schedules new deliveries (`replay_of` links the previous one). `202`. |

URL policy (defaults): `https` only, no credentials or fragments, no
loopback/private/link-local/CGNAT/reserved addresses — checked on registration
and again after DNS resolution at connect time; redirects are not followed.
`GATEWAY_WEBHOOK_ALLOW_HTTP` / `GATEWAY_WEBHOOK_ALLOW_PRIVATE` relax this for
local development.

Event types: `payment_intent.created`, `.requires_action`, `.requires_review`,
`.authorized`, `.captured`, `.declined`, `.blocked`, `.voided`, `.canceled`,
`refund.succeeded`, `refund.failed`, `settlement.created`, `payout.scheduled`,
`payout.paid`, `webhook_endpoint.ping`.

**Delivery format** — `POST` with body

```json
{"id":"evt_…","type":"payment_intent.captured","merchant_id":"mer_…","created_at":"…","data":{ …resource… }}
```

Headers: `Gateway-Event-Id`, `Gateway-Event-Type`, `Gateway-Delivery-Id`,
`Gateway-Delivery-Attempt`, `Gateway-Signature: t=<unix>,v1=<hex>[,v1=<hex>]`.

Verify: `hex(HMAC_SHA256(secret, "<t>.<raw body>"))` equals any `v1` value and
`t` is within your tolerance (e.g. 5 minutes). Go receivers can call
`webhook.Verify`; `gatewayctl verify-signature` does the same from a shell.
Any `2xx` acknowledges; other responses, timeouts and connection errors are
retried with exponential backoff (`base_backoff · 2^(n-1)`, ±20% jitter,
capped at `max_backoff`) up to `max_attempts`, then marked `failed`.
Deliveries are concurrent, so ordering is not guaranteed.

### Settlements and payouts

| Method & path | Description |
|---|---|
| `GET /v1/settlements` | Batches; filter `date=YYYY-MM-DD`. |
| `GET /v1/settlements/{id}` | Batch: gross_captured, refunded, fees, net, counts, status (`payout_scheduled` \| `no_payout`), payout_id. |
| `GET /v1/settlements/{id}/report` | Items + payout as JSON; `?format=csv` for a CSV statement. |
| `GET /v1/payouts` | Payouts (`scheduled` \| `paid`). |

## Operator API (`/admin/v1`)

### Merchants

| Method & path | Description |
|---|---|
| `POST /admin/v1/merchants` | `{"name","email","country","default_currency","activate":bool,"fees":{"percent_bps","fixed_minor","refund_fixed_minor","return_fee_on_refund"}}` → `201`. Email must be unique. |
| `GET /admin/v1/merchants` | List; filter `status`. |
| `GET /admin/v1/merchants/{id}` | Retrieve. |
| `PATCH /admin/v1/merchants/{id}` | `{"name"?, "email"?}`. |
| `POST /admin/v1/merchants/{id}/status` | `{"status":"active"\|"suspended"\|"closed","reason":"…"}`. Transitions: pending→active/closed, active→suspended/closed, suspended→active/closed. Suspended merchants may capture/void/refund but not create or authorize; closing revokes all keys. |
| `GET /admin/v1/merchants/{id}/fee_schedules` | All versions. |
| `POST /admin/v1/merchants/{id}/fee_schedules` | New version (`effective_from` optional, default now). |
| `GET /admin/v1/merchants/{id}/api_keys` | Keys. |
| `POST /admin/v1/merchants/{id}/api_keys` | `{"label","expires_at"?}` → `201` with `secret` (shown once). |
| `POST /admin/v1/merchants/{id}/api_keys/{key_id}/rotate` | `{"grace_period_seconds"?}` → `201` new key. |
| `DELETE /admin/v1/merchants/{id}/api_keys/{key_id}` | Revoke → `204`. |
| `GET /admin/v1/merchants/{id}/balance` | Ledger balances. |

### Payments and fraud

| Method & path | Description |
|---|---|
| `GET /admin/v1/payment_intents` | All payments; filters `merchant_id`, `status`, `created_after`, `created_before`. |
| `GET /admin/v1/payment_intents/{id}` | Retrieve any payment. |
| `POST /admin/v1/payment_intents/{id}/review` | `{"decision":"approve"\|"reject","note":"…"}` for `requires_review` payments. Approve sends the authorization to the network; reject → `blocked`. |
| `GET /admin/v1/fraud/rules` | Active rules and thresholds. |
| `GET /admin/v1/fraud/blocklist` | Blocked BINs. |
| `POST /admin/v1/fraud/blocklist` | `{"bin":"6 digits","reason":"…"}` → `201`. |
| `DELETE /admin/v1/fraud/blocklist/{bin}` | `204`. |

### Settlement, payouts, reconciliation

| Method & path | Description |
|---|---|
| `POST /admin/v1/settlements/run` | `{"date":"YYYY-MM-DD","merchant_id"?,"allow_open_day"?}`. Only finished UTC days unless `allow_open_day`. Returns batches created; re-running settles only new items. |
| `GET /admin/v1/settlements` | Filters `merchant_id`, `date`. |
| `GET /admin/v1/settlements/summary?date=` | Totals per currency (optional `merchant_id`). |
| `GET /admin/v1/settlements/{id}/report` | JSON or `?format=csv`. |
| `POST /admin/v1/payouts/pay_due` | Mark scheduled payouts due today or earlier as paid. |
| `GET /admin/v1/acquirer/settlement_file?date=` | The simulated acquirer's settlement CSV (`record_type,acquirer_ref,parent_ref,merchant_id,amount,currency,transaction_time`). |
| `POST /admin/v1/reconciliations` | `{"date"}` — reconcile the ledger against the simulator file. `201`. |
| `POST /admin/v1/reconciliations/upload?date=` | `Content-Type: text/csv` body in the format above. Files containing a PAN are rejected. `201`. |
| `GET /admin/v1/reconciliations` / `{id}` | Runs; a run includes `discrepancies` (`missing_at_acquirer`, `missing_internally`, `amount_mismatch`, `currency_mismatch`, `type_mismatch`, `duplicate_at_acquirer`). |

### Audit, vault, ledger, jobs

| Method & path | Description |
|---|---|
| `GET /admin/v1/audit_logs` | Ascending by `seq`; filters `merchant_id`, `resource_type`, `resource_id`, `action`, `actor_id`, `since` (RFC 3339), `after_seq`, `limit` ≤ 1000. |
| `GET /admin/v1/audit_logs/verify` | Recompute the hash chain: `{"valid":true,"entries_checked":n,"head_hash":"…"}` or the first broken `seq`. |
| `GET /admin/v1/vault/status` | Primary key id, configured key ids, cards per key. |
| `POST /admin/v1/vault/rewrap` | Re-encrypt cards sealed with older keys under the primary key. |
| `GET /admin/v1/ledger/trial_balance` | Debits vs credits per currency. |
| `POST /admin/v1/jobs/{name}/run` | Run a job now: `expire_authorizations`, `purge_idempotency`, `pay_payouts`, `dispatch_webhooks`, `settle_previous_day`. |

## Operational endpoints

| Method & path | Auth | Description |
|---|---|---|
| `GET /healthz` | none | Liveness + version. |
| `GET /readyz` | none | Database reachability (`503` when unavailable). |
| `GET /metrics` | admin | Prometheus text format: HTTP requests/latency by route, payment operations, acquirer latency, fraud decisions, webhook attempts/latency, rate-limit rejections, settlement batches, reconciliations, idempotent replays, job runs, payment and delivery gauges. |
| `GET /v1/test_cards` | none (IP rate-limited) | Synthetic test cards and event types. |
