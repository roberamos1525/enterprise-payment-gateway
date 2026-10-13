# Testing

## Commands

```bash
go test ./... -count=1           # full suite
go test ./tests/integration -v   # end-to-end flows only
gofmt -l . && go vet ./...       # formatting and static checks
bash examples/local-stack.sh     # live smoke test with real binaries
```

Recorded results on the development machine (Windows 11, Go 1.26.2):
`go test ./... -count=1` → **141 tests passed, 0 failed, 29 packages**
(run twice); `gofmt -l .` → no output; `go vet ./...` → clean. The race
detector was **not** run locally because CGO is unavailable there; the CI
workflow runs `-race` on Linux (that workflow has not been executed).

## Strategy

| Layer | Location | Approach |
|---|---|---|
| Pure logic | `internal/{money,card,apperr,ids,clock,ratelimit,metrics,fraud,reconcile(Match)}` | Table-driven tests, edge cases (rounding modes, overflow, Luhn, BIN ranges, every discrepancy kind). |
| Persistence | `internal/store`, `internal/vault`, `internal/audit`, `internal/ledger`, `internal/idempotency`, `internal/acquirer`, `internal/webhook` | Real SQLite in `t.TempDir()`: migrations, optimistic locking, rollback, append-only triggers, lease claiming, key rotation/re-wrap, tamper detection. |
| Services | `internal/{payment,settlement,merchant,app,cli,httpapi}` (external `_test` packages) | `tests/testenv` builds the complete application with a fake clock, zero acquirer latency and an `httptest` server. |
| End to end | `tests/integration` | HTTP-only flows: full lifecycle with signed webhook delivery to an `httptest` receiver, 3DS, declines + retry, fraud block/review, velocity, void/cancel/expiry, refund failures, reconciliation (injected + uploaded files), idempotency (sequential and concurrent), auth/isolation, key rotation, merchant lifecycle, rate limits, error envelopes, pagination, webhook retries/backoff/replay/secret rotation, restart with vault key rotation, PAN-leak checks on responses, logs and DB files. |
| Binaries | `cmd/webhook-receiver` | Handler test for signature verification and storage. |

## Determinism

- Time is injected (`clock.Fake`): authorization expiry, settlement days,
  payout dates, idempotency TTLs, webhook backoff and velocity windows are
  driven by advancing the fake clock instead of sleeping.
- The acquirer's outcomes are a pure function of the test PAN; its latency is
  zero in tests (and a test substitutes the sleep function to check latency
  and timeout handling).
- Discrepancy injection is seeded and hash-based, so reconciliation tests are
  repeatable.
- Webhook dispatch is driven synchronously with `Dispatcher.RunOnce` /
  `testenv.Env.DispatchAll`.

## Smoke test

`examples/local-stack.sh` builds the binaries, starts the gateway with
throwaway secrets, and runs `examples/curl-walkthrough.sh`, which onboards a
merchant, issues a key, registers a webhook endpoint, starts the
signature-verifying receiver, authorizes (and replays idempotently), partially
captures, refunds, shows a decline, settles the open day, prints the CSV
report, reconciles, waits for webhook deliveries and verifies the audit chain
and trial balance; finally it checks the gateway log for raw PANs. This was run
successfully on the development machine (Windows 11, Git Bash).

## Writing new tests

- Prefer `testenv.New(t, mutate...)` for anything that needs more than one
  package; use the response helpers (`Must`, `Str`, `Num`, `ErrType`).
- Use synthetic PANs from `internal/card` (`card.TestCards()`), never real ones.
- Assert on error codes (`apperr.Is`) rather than messages.
