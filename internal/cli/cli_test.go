package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/cli"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

func run(t *testing.T, vars map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Run(args, cli.Env{Stdout: &out, Stderr: &errOut, Getenv: func(k string) string { return vars[k] }})
	return code, out.String(), errOut.String()
}

func TestLocalCommands(t *testing.T) {
	code, out, _ := run(t, nil)
	if code != 0 || !strings.Contains(out, "gatewayctl") {
		t.Fatalf("usage: %d %s", code, out)
	}
	code, out, _ = run(t, nil, "testcards")
	if code != 0 || !strings.Contains(out, "4242424242424242") || !strings.Contains(out, "decline_insufficient_funds") {
		t.Fatalf("testcards: %s", out)
	}
	code, out, _ = run(t, nil, "gen-pan", "-prefix", "510000", "-n", "3")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 3 {
		t.Fatalf("gen-pan: %d %q", code, out)
	}
	for _, l := range lines {
		pan := strings.Fields(l)[0]
		if !card.LuhnValid(pan) || !strings.HasPrefix(pan, "510000") || !strings.HasSuffix(l, "mastercard") {
			t.Fatalf("generated %q", l)
		}
	}
	if code, _, _ := run(t, nil, "gen-pan", "-n", "0"); code != 2 {
		t.Fatalf("bad -n exit code %d", code)
	}
	code, out, _ = run(t, nil, "gen-key", "-id", "k7")
	if code != 0 {
		t.Fatal("gen-key failed")
	}
	if _, err := vault.ParseKeyring(strings.TrimSpace(out), "fingerprint-secret-xyz"); err != nil || !strings.HasPrefix(out, "k7:") {
		t.Fatalf("generated key unusable: %q %v", out, err)
	}
	if code, _, errOut := run(t, nil, "launch-rockets"); code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("unknown command: %d", code)
	}
}

func TestVerifySignatureCommand(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"payment_intent.captured"}`)
	path := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	header := webhook.SignatureHeader(1_700_000_000, body, "whsec_test")
	code, out, _ := run(t, nil, "verify-signature", "-secret", "whsec_test", "-header", header, "-body", path)
	if code != 0 || !strings.Contains(out, "signature valid") {
		t.Fatalf("valid signature: %d %s", code, out)
	}
	code, _, errOut := run(t, map[string]string{"WEBHOOK_SECRET": "whsec_other"}, "verify-signature", "-header", header, "-body", path)
	if code != 1 || !strings.Contains(errOut, "no matching signature") {
		t.Fatalf("wrong secret: %d %s", code, errOut)
	}
	if code, _, _ := run(t, nil, "verify-signature", "-header", header); code != 2 {
		t.Fatalf("missing args: %d", code)
	}
}

func TestAdminCommandsAgainstLiveServer(t *testing.T) {
	env := testenv.New(t)
	vars := map[string]string{"GATEWAY_URL": env.Server.URL, "GATEWAY_ADMIN_TOKEN": testenv.AdminToken}

	code, out, errOut := run(t, vars, "merchant-create", "-name", "CLI Shop", "-email", "cli@shop.test", "-activate")
	if code != 0 {
		t.Fatalf("merchant-create: %d %s", code, errOut)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil || m["status"] != "active" {
		t.Fatalf("merchant: %s %v", out, err)
	}
	code, out, _ = run(t, vars, "key-create", "-merchant", m["id"].(string))
	if code != 0 || !strings.Contains(out, `"secret": "sk_test_`) {
		t.Fatalf("key-create: %s", out)
	}
	if code, _, _ := run(t, vars, "key-create"); code != 2 {
		t.Fatal("missing -merchant should be a usage error")
	}
	code, out, _ = run(t, vars, "settle", "-date", "2026-03-01")
	if code != 0 || !strings.Contains(out, `"settlement_date": "2026-03-01"`) {
		t.Fatalf("settle: %s", out)
	}
	code, out, _ = run(t, vars, "reconcile", "-date", "2026-03-01")
	if code != 0 || !strings.Contains(out, `"status": "balanced"`) {
		t.Fatalf("reconcile: %s", out)
	}
	csvPath := filepath.Join(t.TempDir(), "acq.csv")
	_, fileOut, _ := run(t, vars, "acquirer-file", "-date", "2026-03-01")
	if err := os.WriteFile(csvPath, []byte(fileOut), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, vars, "reconcile", "-date", "2026-03-01", "-file", csvPath)
	if code != 0 || !strings.Contains(out, `"source": "upload"`) {
		t.Fatalf("reconcile upload: %s", out)
	}
	code, out, _ = run(t, vars, "audit-verify")
	if code != 0 || !strings.Contains(out, `"valid": true`) {
		t.Fatalf("audit-verify: %s", out)
	}
	code, out, _ = run(t, vars, "job", "purge_idempotency")
	if code != 0 || !strings.Contains(out, "idempotency_purged") {
		t.Fatalf("job: %s", out)
	}
	code, out, _ = run(t, vars, "get", "/admin/v1/merchants")
	if code != 0 || !strings.Contains(out, "CLI Shop") {
		t.Fatalf("get: %s", out)
	}
	// API errors surface the envelope; a missing token is a usage error.
	code, _, errOut = run(t, vars, "job", "no_such_job")
	if code != 1 || !strings.Contains(errOut, "not_found") {
		t.Fatalf("bad job: %d %s", code, errOut)
	}
	if code, _, _ := run(t, map[string]string{"GATEWAY_URL": env.Server.URL}, "audit-verify"); code != 2 {
		t.Fatalf("missing token: %d", code)
	}
}
