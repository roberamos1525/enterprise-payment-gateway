package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

// receiver is an httptest webhook endpoint that verifies signatures.
type receiver struct {
	mu       sync.Mutex
	secret   string
	events   []webhook.Envelope
	failures int // respond 500 to this many requests first
	badSigs  int
	srv      *httptest.Server
	now      func() time.Time
}

func newReceiver(t *testing.T, now func() time.Time) *receiver {
	r := &receiver{now: now}
	r.srv = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.srv.Close)
	return r
}

func (rc *receiver) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if err := webhook.Verify(r.Header.Get(webhook.HeaderSignature), body, rc.secret, 5*time.Minute, rc.now()); err != nil {
		rc.badSigs++
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	if rc.failures > 0 {
		rc.failures--
		http.Error(w, "try later", http.StatusInternalServerError)
		return
	}
	var env webhook.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	rc.events = append(rc.events, env)
	w.WriteHeader(http.StatusOK)
}

func (rc *receiver) types() []string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := make([]string, len(rc.events))
	for i, e := range rc.events {
		out[i] = e.Type
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestFullPaymentLifecycle drives the complete happy path over HTTP:
// onboarding, keys, webhook endpoint, authorize, partial capture, two partial
// refunds, settlement, payout, reconciliation and signed webhook delivery.
func TestFullPaymentLifecycle(t *testing.T) {
	env := testenv.New(t)
	admin := testenv.AdminToken
	rc := newReceiver(t, env.Clock.Now)

	// Onboard a merchant and issue a key via the admin API.
	m := env.Must(env.Do("POST", "/admin/v1/merchants", admin, map[string]any{
		"name": "Flow Shop", "email": "flow@shop.test", "country": "US", "default_currency": "usd", "activate": true,
		"fees": map[string]any{"percent_bps": 250, "fixed_minor": 25, "refund_fixed_minor": 10},
	}), http.StatusCreated)
	mid := m.Str("id")
	key := env.Must(env.Do("POST", "/admin/v1/merchants/"+mid+"/api_keys", admin, map[string]any{"label": "ci"}), http.StatusCreated)
	sk := key.Str("secret")
	if !strings.HasPrefix(sk, "sk_test_") {
		t.Fatalf("unexpected key format %q", sk)
	}

	// Register a webhook endpoint pointing at the local receiver.
	ep := env.Must(env.Do("POST", "/v1/webhook_endpoints", sk, map[string]any{
		"url": rc.srv.URL + "/hooks", "events": []string{"*"},
	}), http.StatusCreated)
	rc.secret = ep.Str("secret")

	// Create and confirm a payment in one call.
	pi := env.Must(env.Do("POST", "/v1/payment_intents", sk, map[string]any{
		"amount": 12000, "currency": "USD", "description": "order #1", "metadata": map[string]string{"order": "1"},
		"confirm": true, "payment_method": testenv.Card("4242 4242 4242 4242"),
	}, "Idempotency-Key", "order-1"), http.StatusCreated)
	if pi.Str("status") != "authorized" {
		t.Fatalf("status = %s", pi.Str("status"))
	}
	pid := pi.Str("id")

	// Partial capture, then two partial refunds.
	cap := env.Must(env.Do("POST", "/v1/payment_intents/"+pid+"/capture", sk, map[string]any{"amount": 10000}), http.StatusOK)
	if cap.Str("status") != "captured" || cap.Num("amount_captured") != 10000 {
		t.Fatalf("capture: %s", cap.Raw)
	}
	r1 := env.Must(env.Do("POST", "/v1/payment_intents/"+pid+"/refunds", sk, map[string]any{"amount": 2500, "reason": "requested_by_customer"}), http.StatusCreated)
	if r1.Str("status") != "succeeded" {
		t.Fatalf("refund 1: %s", r1.Raw)
	}
	env.Must(env.Do("POST", "/v1/payment_intents/"+pid+"/refunds", sk, map[string]any{"amount": 7500}), http.StatusCreated)
	final := env.Must(env.Do("GET", "/v1/payment_intents/"+pid, sk, nil), http.StatusOK)
	if final.Str("status") != "refunded" || final.Num("amount_refundable") != 0 {
		t.Fatalf("final payment: %s", final.Raw)
	}
	// Nothing left to refund.
	over := env.Do("POST", "/v1/payment_intents/"+pid+"/refunds", sk, map[string]any{"amount": 1})
	if over.Status != http.StatusConflict {
		t.Fatalf("refund on refunded payment: %d %s", over.Status, over.Raw)
	}

	// A second, untouched capture so the batch has a positive net.
	pi2 := env.Must(env.Do("POST", "/v1/payment_intents", sk, map[string]any{
		"amount": 5000, "currency": "USD", "capture_method": "automatic", "confirm": true,
		"payment_method": testenv.Card("5555555555554444"),
	}), http.StatusCreated)
	if pi2.Str("status") != "captured" {
		t.Fatalf("automatic capture: %s", pi2.Raw)
	}

	// The day must end before it can be settled without allow_open_day.
	today := env.Clock.Now().Format(clock.DateLayout)
	early := env.Do("POST", "/admin/v1/settlements/run", admin, map[string]any{"date": today})
	if early.Status != http.StatusBadRequest {
		t.Fatalf("settling an open day should fail: %d %s", early.Status, early.Raw)
	}
	env.Clock.Advance(16 * time.Hour)
	run := env.Must(env.Do("POST", "/admin/v1/settlements/run", admin, map[string]any{"date": today}), http.StatusOK)
	batches := run.JSON["batches"].([]any)
	if len(batches) != 1 {
		t.Fatalf("batches: %s", run.Raw)
	}
	b := batches[0].(map[string]any)
	// fees: capture 10000 -> 250+25=275; capture 5000 -> 125+25=150; refunds 2 x 10 = 20.
	gross, refunded, fees, net := int64(b["gross_captured"].(float64)), int64(b["refunded"].(float64)),
		int64(b["fees"].(float64)), int64(b["net"].(float64))
	if gross != 15000 || refunded != 10000 || fees != 445 || net != 4555 {
		t.Fatalf("batch totals gross=%d refunded=%d fees=%d net=%d", gross, refunded, fees, net)
	}
	// Re-running settles nothing new.
	again := env.Must(env.Do("POST", "/admin/v1/settlements/run", admin, map[string]any{"date": today}), http.StatusOK)
	if len(again.JSON["batches"].([]any)) != 0 {
		t.Fatalf("settlement is not idempotent: %s", again.Raw)
	}

	// The merchant can read the CSV statement.
	csvResp := env.Must(env.Do("GET", "/v1/settlements/"+b["id"].(string)+"/report?format=csv", sk, nil), http.StatusOK)
	if !strings.Contains(string(csvResp.Raw), "TOTAL") || !strings.Contains(string(csvResp.Raw), pid) {
		t.Fatalf("csv report: %s", csvResp.Raw)
	}

	// Payout becomes due after the configured delay and is paid.
	env.Clock.Advance(24 * time.Hour)
	paid := env.Must(env.Do("POST", "/admin/v1/payouts/pay_due", admin, map[string]any{}), http.StatusOK)
	if paid.Num("payouts_paid") != 1 {
		t.Fatalf("pay due: %s", paid.Raw)
	}

	// Reconciliation of the day against the simulated acquirer is balanced.
	rec := env.Must(env.Do("POST", "/admin/v1/reconciliations", admin, map[string]any{"date": today}), http.StatusCreated)
	if rec.Str("status") != "balanced" || rec.Num("matched_count") != 4 {
		t.Fatalf("reconciliation: %s", rec.Raw)
	}

	// Ledger: the merchant's payable is fully paid out, trial balance holds.
	bal := env.Must(env.Do("GET", "/v1/balance", sk, nil), http.StatusOK)
	for _, a := range bal.JSON["accounts"].([]any) {
		acc := a.(map[string]any)
		if acc["account"] == "merchant_payable" && acc["balance"].(float64) != 0 {
			t.Fatalf("merchant payable should be zero after payout: %v", acc)
		}
	}
	tb := env.Must(env.Do("GET", "/admin/v1/ledger/trial_balance", admin, nil), http.StatusOK)
	if tb.JSON["balanced"] != true {
		t.Fatalf("trial balance: %s", tb.Raw)
	}

	// Deliver webhooks and check the receiver saw the lifecycle, signed.
	env.DispatchAll()
	got := rc.types()
	for _, want := range []string{"payment_intent.created", "payment_intent.authorized", "payment_intent.captured",
		"refund.succeeded", "settlement.created", "payout.scheduled", "payout.paid"} {
		if !contains(got, want) {
			t.Errorf("receiver missing %s (got %v)", want, got)
		}
	}
	if rc.badSigs != 0 {
		t.Fatalf("%d webhook signatures failed verification", rc.badSigs)
	}

	// The audit chain verifies.
	ver := env.Must(env.Do("GET", "/admin/v1/audit_logs/verify", admin, nil), http.StatusOK)
	if ver.JSON["valid"] != true {
		t.Fatalf("audit verify: %s", ver.Raw)
	}
	hist := env.Must(env.Do("GET", "/v1/payment_intents/"+pid+"/history", sk, nil), http.StatusOK)
	if n := len(hist.JSON["data"].([]any)); n != 5 { // create, authorize, capture, refund, refund
		t.Fatalf("history entries = %d: %s", n, hist.Raw)
	}
}

// TestNoPANLeaks checks that the raw PAN never appears in API responses,
// logs, the audit log or the database files.
func TestNoPANLeaks(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Leak Check")
	const pan = "4000002760000016"
	var bodies []string
	r := env.Must(env.Do("POST", "/v1/tokens", m.Key, map[string]any{"card": testenv.Card(pan)["card"]}), http.StatusCreated)
	bodies = append(bodies, string(r.Raw))
	pi := env.Must(env.Do("POST", "/v1/payment_intents", m.Key, map[string]any{
		"amount": 999, "currency": "EUR", "confirm": true, "capture_method": "automatic",
		"payment_method": map[string]any{"token": r.Str("token")},
	}), http.StatusCreated)
	bodies = append(bodies, string(pi.Raw))
	ref := env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/refunds", m.Key, nil), http.StatusCreated)
	bodies = append(bodies, string(ref.Raw))
	// A validation failure that includes the PAN in the request.
	bad := env.Do("POST", "/v1/tokens", m.Key, map[string]any{"card": map[string]any{"number": pan, "exp_month": 13, "exp_year": 2030, "cvc": "1"}})
	bodies = append(bodies, string(bad.Raw))
	audit := env.Must(env.Do("GET", "/admin/v1/audit_logs", testenv.AdminToken, nil), http.StatusOK)
	bodies = append(bodies, string(audit.Raw))
	events := env.Must(env.Do("GET", "/v1/events", m.Key, nil), http.StatusOK)
	bodies = append(bodies, string(events.Raw))

	for i, b := range bodies {
		if strings.Contains(b, pan) {
			t.Fatalf("response %d leaks the PAN: %s", i, b)
		}
	}
	if strings.Contains(env.Logs.String(), pan) {
		t.Fatal("logs leak the PAN")
	}
	if !strings.Contains(string(r.Raw), "400000******0016") {
		t.Fatalf("expected masked card in token response: %s", r.Raw)
	}
	// Database files (main + WAL) contain only ciphertext.
	for _, f := range []string{env.DBPath, env.DBPath + "-wal"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), pan) {
			t.Fatalf("%s contains the raw PAN", f)
		}
	}
}
