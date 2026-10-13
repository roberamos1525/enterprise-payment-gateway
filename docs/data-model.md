# Data model

Schema: `internal/store/migrations/0001_init.sql`, applied by the embedded
migration runner (`schema_migrations` tracks the version; migrations must be
numbered contiguously and the binary refuses databases newer than itself).

Conventions: timestamps are `INTEGER` Unix nanoseconds (UTC); money columns
are `INTEGER` minor units next to a `currency` column; ids are prefixed,
time-ordered strings (`pi_`, `mer_`, `re_` …) so `ORDER BY id` is creation
order and serves as the pagination cursor.

## Tables

| Table | Purpose | Notable constraints |
|---|---|---|
| `merchants` | Merchant accounts | unique `email`; `status` check |
| `fee_schedules` | Versioned pricing (`percent_bps`, `fixed_minor`, `refund_fixed_minor`, `return_fee_on_refund`, `effective_from`) | bps 0–10000 |
| `api_keys` | `prefix` (unique), `hash` (SHA-256), status, expiry, rotation link, last use | no secret stored |
| `webhook_endpoints` | URL, subscribed `events` (JSON), sealed current/previous signing secret, rotation expiry | — |
| `vault_cards` | `token`, merchant, `fingerprint`, `key_id`, AES-GCM `ciphertext`, BIN, last4, brand, expiry, country, funding | the only place a PAN exists (encrypted) |
| `payment_intents` | Aggregate root: amounts (authorized/captured/refunded), status, card summary, acquirer refs, decline info, risk score/action/reasons, metadata, `version` | `amount_captured ≤ amount_authorized`, `amount_refunded ≤ amount_captured` |
| `payment_history` | Every transition (action, from, to, amount, detail) | — |
| `refunds` | Refunds incl. failed ones and acquirer reference | `amount > 0` |
| `idempotency_keys` | (merchant, key) → fingerprint, status, stored response, expiry, lock | PK (merchant_id, key) |
| `events` | Immutable domain events = webhook outbox | — |
| `webhook_deliveries` | One per (event, endpoint, replay): status, attempts, `next_attempt_at`, lease, last result | index on (status, next_attempt_at) |
| `webhook_attempts` | Delivery log per attempt | — |
| `ledger_journals` / `ledger_entries` | Double-entry journals and lines | unique (kind, ref); debit/credit ≥ 0 |
| `settlement_batches` | Per merchant/day/currency totals, fee schedule used, status | — |
| `settlement_items` | Captures/refunds included in a batch | PK (kind, ref_id): an item settles once |
| `payouts` | Net amount owed, `scheduled_for`, paid time | one per batch |
| `acquirer_transactions` | The simulated acquirer's own books (authorization/capture/void/refund) | — |
| `reconciliation_runs` / `reconciliation_discrepancies` | Reconciliation results | — |
| `fraud_bin_blocklist` | Blocked BINs | PK bin |
| `audit_logs` | Append-only, hash-chained audit trail | triggers abort UPDATE/DELETE |

## Relationships

```
merchants 1─* fee_schedules
merchants 1─* api_keys
merchants 1─* webhook_endpoints 1─* webhook_deliveries *─1 events
merchants 1─* vault_cards
merchants 1─* payment_intents 1─* payment_history
                              1─* refunds
payment_intents/refunds ──(acquirer refs)── acquirer_transactions
settlement_batches 1─* settlement_items, 1─0..1 payouts
ledger_journals 1─* ledger_entries   (ref = acquirer ref, batch id or payout id)
reconciliation_runs 1─* reconciliation_discrepancies
```

## Ledger journal kinds

| Kind | Ref | Entries |
|---|---|---|
| `capture` | acquirer capture ref | Dr acquirer_receivable / Cr merchant_payable |
| `refund` | acquirer refund ref | Dr merchant_payable / Cr acquirer_receivable |
| `settlement_fee` | batch id | Dr merchant_payable / Cr fee_revenue (reversed when net fees are negative) |
| `payout` | payout id | Dr merchant_payable / Cr payouts_pending |
| `payout_paid` | payout id | Dr payouts_pending / Cr cash |

Reconciliation joins `capture`/`refund` journals to acquirer file rows by
reference.

## Audit hash chain

`hash = SHA-256(len:prev_hash | len:id | len:created_at_ns | len:actor_type |
len:actor_id | len:action | len:resource_type | len:resource_id |
len:merchant_id | len:request_id | len:ip | len:metadata)` — fields are
length-prefixed so boundaries cannot be shifted. The first entry links to a
64-zero genesis hash.
