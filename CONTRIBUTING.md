# Contributing

Thanks for your interest! This is a simulation/portfolio project; contributions
must keep it that way — **synthetic data only**, no real card numbers, no
connections to real payment networks, no secrets in the repository.

## Development setup

```bash
go version            # 1.26+
go build ./...
go test ./... -count=1
```

No C compiler, Docker or external services are required.

## Before opening a change

1. `gofmt -l .` prints nothing (run `gofmt -w .`).
2. `go vet ./...` is clean.
3. `go test ./... -count=1` passes; run it twice if you touched concurrency.
4. On Linux/macOS with CGO available, also run `go test ./... -race -count=1`.
5. `go mod tidy` leaves `go.mod`/`go.sum` unchanged (or commit the changes).
6. If you changed endpoints, update [API.md](API.md); if you changed
   behaviour, update the relevant docs and [CHANGELOG.md](CHANGELOG.md).

## Code guidelines

- **Money** is `int64` minor units plus an ISO 4217 currency; never floats.
  Use `internal/money` for arithmetic, formatting and fee rounding.
- **Errors** returned to clients are `*apperr.Error` with a stable code; wrap
  internal causes with `apperr.Wrap`/`apperr.Internal` so they are logged but
  not exposed.
- **Validation** goes through `apperr.Validator` so all field errors are
  reported at once.
- **State changes** of a payment must go through `payment.Service.commit`,
  which validates the transition table and writes history, events, ledger and
  audit atomically.
- **Side effects** that must survive a crash (webhooks) are written to the
  outbox inside the same transaction — never sent directly from a handler.
- **Card data**: only `vault` may decrypt PANs and only `acquirer` may ask it
  to. Never log or return a PAN, CVC, API key secret or webhook secret.
- **Time**: use the injected `clock.Clock`, not `time.Now()`, in domain code so
  tests can control it.
- Keep packages small and dependency direction downward
  (`httpapi → services → store → model`).

## Tests

- Unit tests sit next to the code (`*_test.go`). Service-level tests that need
  the whole application use `tests/testenv` from an external test package
  (`package foo_test`).
- End-to-end flows go in `tests/integration` and drive the HTTP API.
- Use the fake clock instead of sleeping; use `httptest` servers for webhook
  receivers; use the synthetic test cards from `internal/card`.

## Commit messages

Use the imperative mood and explain *why* in the body when it is not obvious,
e.g. `settlement: settle late items as a supplementary batch`.
