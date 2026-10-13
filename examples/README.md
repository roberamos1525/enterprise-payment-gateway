# Examples

All examples use synthetic test data only.

| Path | What it shows |
|---|---|
| `local-stack.sh` | Builds the binaries, starts the gateway with throwaway secrets, runs the walkthrough (which launches the webhook receiver) and stops everything. `bash examples/local-stack.sh` from the repo root. |
| `curl-walkthrough.sh` | The full flow with curl against a running gateway: onboard → key → webhook endpoint → authorize (idempotent replay) → partial capture → refund → decline → settlement + CSV report → reconciliation → webhook deliveries → audit/ledger checks. Needs `GATEWAY_URL` and `GATEWAY_ADMIN_TOKEN`. |
| `go-client/` | Standard-library Go client: tokenize a 3-D Secure test card, authorize, complete the challenge, capture, refund. `GATEWAY_API_KEY=sk_test_… go run ./examples/go-client`. |
| `requests/*.json` | Sample request bodies for the main endpoints (`curl -d @examples/requests/partial-refund.json …`). |
| `config/gateway.example.json` | Every configuration option with its default; load with `gateway -config`. Secrets still come from the environment. |
| `settlement-file-sample.csv` | The acquirer settlement-file format accepted by `POST /admin/v1/reconciliations/upload?date=…`. |
