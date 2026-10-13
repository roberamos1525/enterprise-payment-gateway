# Payment lifecycle

The payment-intent state machine is defined as a table in
`internal/payment/statemachine.go`; every write validates
`(from status, action, to status)` against it.

```
created ──authorize──┬──▶ authorized ──capture──▶ captured ──refund──▶ partially_refunded ──refund──▶ refunded
   ▲                 │        │                      └────────────── refund (remaining amount) ──────────▲
   │                 │        └── void / expire ──▶ voided
   │                 ├──▶ declined ──authorize (retry with another card)──▶ …
   │                 ├──▶ requires_action ──complete_3ds──▶ authorized | declined
   │                 ├──▶ requires_review ──review_approve──▶ authorized | requires_action | declined
   │                 │                    └─review_reject──▶ blocked
   │                 └──▶ blocked (fraud engine)
   │
created / requires_action / requires_review / declined ──cancel──▶ canceled
```

| Action | From | To |
|---|---|---|
| `authorize` | created, declined | authorized, requires_action, requires_review, declined, blocked |
| `complete_3ds` | requires_action | authorized, declined |
| `review_approve` | requires_review | authorized, requires_action, declined |
| `review_reject` | requires_review | blocked |
| `capture` | authorized | captured |
| `void` | authorized | voided |
| `expire` (system job) | authorized | voided |
| `cancel` | created, requires_action, requires_review, declined | canceled |
| `refund` | captured, partially_refunded | partially_refunded, refunded |

Terminal states: `refunded`, `voided`, `canceled`, `blocked`.

## Authorization sequence

1. Merchant must be `active`; the payment must allow `authorize`.
2. The card is validated and tokenized (or an existing merchant-owned token
   is looked up; an expired token is declined with `expired_card`).
3. The fraud engine scores the attempt (card fingerprint, BIN, card country,
   billing country, client IP, amount, currency):
   - `block` → status `blocked`, `decline_code: fraud_blocked`;
   - `review` → status `requires_review` (no network call yet);
   - `allow` → continue.
4. The simulated acquirer reveals the PAN in memory, picks the test-card
   scenario and simulates latency. Outcomes:
   - 3DS required → `requires_action` with `next_action.challenge_id`;
   - approved → `authorized` with `authorization_code`, `acquirer_reference`
     and `authorization_expires_at = now + authorization_ttl`;
   - declined → `declined` with `decline_code`/`decline_message`;
   - timeout → `503 service_unavailable`, the payment is unchanged.
5. `capture_method: automatic` captures the full amount right away.

Each step that changes state writes the payment row, a history entry, the
outbox event (`payment_intent.<status>`), and an audit entry in one
transaction.

## Capture, void, refund

- **Capture** (`authorized` only): amount ≤ authorized, one capture per
  payment, remainder released (recorded in the history detail). Ledger:
  `acquirer_receivable` Dr / `merchant_payable` Cr.
- **Void** (`authorized` only): releases the hold at the acquirer.
- **Expire**: the `expire_authorizations` job voids authorizations past
  `authorization_expires_at` with `cancellation_reason: authorization_expired`.
- **Refund** (`captured`/`partially_refunded`): any amount ≤ refundable,
  repeatedly. Ledger: `merchant_payable` Dr / `acquirer_receivable` Cr. A
  refund rejected by the acquirer is stored as `failed` (`refund.failed`
  event) and does not change the payment.

## Concurrency

Operations on one payment are serialised by an in-process keyed mutex; the
`version` column provides optimistic locking at the database level. Tests
(`TestConcurrentCapturesOnlyOneWins`, `TestConcurrentIdempotentRequests`)
fire parallel requests and assert exactly one effect.
