package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

func createIntent(env *testenv.Env, key string, amount int64, extra map[string]any) testenv.Response {
	body := map[string]any{"amount": amount, "currency": "USD"}
	for k, v := range extra {
		body[k] = v
	}
	return env.Must(env.Do("POST", "/v1/payment_intents", key, body), http.StatusCreated)
}

func TestIdempotentCreateOverHTTP(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Idem Shop")
	body := map[string]any{"amount": 1500, "currency": "USD"}
	first := env.Must(env.Do("POST", "/v1/payment_intents", m.Key, body, "Idempotency-Key", "k-1"), http.StatusCreated)
	second := env.Must(env.Do("POST", "/v1/payment_intents", m.Key, body, "Idempotency-Key", "k-1"), http.StatusCreated)
	if first.Str("id") != second.Str("id") {
		t.Fatalf("replay returned a different payment: %s vs %s", first.Str("id"), second.Str("id"))
	}
	if second.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("replayed response not marked")
	}
	// Same key, different payload.
	diff := env.Do("POST", "/v1/payment_intents", m.Key, map[string]any{"amount": 1600, "currency": "USD"}, "Idempotency-Key", "k-1")
	if diff.Status != http.StatusUnprocessableEntity || diff.ErrType() != "idempotency_key_reused" {
		t.Fatalf("mismatch: %d %s", diff.Status, diff.Raw)
	}
	// Keys are scoped per merchant.
	other := env.Onboard("Other Shop")
	o := env.Must(env.Do("POST", "/v1/payment_intents", other.Key, body, "Idempotency-Key", "k-1"), http.StatusCreated)
	if o.Str("id") == first.Str("id") {
		t.Fatal("idempotency keys leaked across merchants")
	}
	// Validation errors are cached too (4xx is a final answer).
	bad := map[string]any{"amount": -1, "currency": "USD"}
	b1 := env.Do("POST", "/v1/payment_intents", m.Key, bad, "Idempotency-Key", "k-bad")
	b2 := env.Do("POST", "/v1/payment_intents", m.Key, bad, "Idempotency-Key", "k-bad")
	if b1.Status != http.StatusUnprocessableEntity || b2.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("validation replay: %d %d", b1.Status, b2.Status)
	}
	// Invalid key format.
	inv := env.Do("POST", "/v1/payment_intents", m.Key, body, "Idempotency-Key", strings.Repeat("x", 300))
	if inv.Status != http.StatusUnprocessableEntity {
		t.Fatalf("long key: %d", inv.Status)
	}
	// After the TTL the key may be reused for a new request.
	env.Clock.Advance(25 * time.Hour)
	third := env.Must(env.Do("POST", "/v1/payment_intents", m.Key, map[string]any{"amount": 1600, "currency": "USD"},
		"Idempotency-Key", "k-1"), http.StatusCreated)
	if third.Str("id") == first.Str("id") {
		t.Fatal("expired key should not replay")
	}
}

func TestThreeDSecureFlows(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("3DS Shop")
	pi := createIntent(env, m.Key, 4200, nil)
	auth := env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/authorize", m.Key, testenv.Card("4000000000003220")), http.StatusOK)
	if auth.Str("status") != "requires_action" {
		t.Fatalf("expected requires_action: %s", auth.Raw)
	}
	next := auth.JSON["next_action"].(map[string]any)
	challenge := next["challenge_id"].(string)
	// Wrong challenge id is rejected.
	wrong := env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/3ds", m.Key, map[string]any{"challenge_id": "3ds_nope", "outcome": "success"})
	if wrong.Status != http.StatusBadRequest {
		t.Fatalf("wrong challenge: %d", wrong.Status)
	}
	ok := env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/3ds", m.Key, map[string]any{"challenge_id": challenge, "outcome": "success"}), http.StatusOK)
	if ok.Str("status") != "authorized" {
		t.Fatalf("after 3ds: %s", ok.Raw)
	}

	// Challenge passed but issuer declines afterwards.
	pi2 := createIntent(env, m.Key, 4200, nil)
	a2 := env.Must(env.Do("POST", "/v1/payment_intents/"+pi2.Str("id")+"/authorize", m.Key, testenv.Card("4000008400001629")), http.StatusOK)
	c2 := a2.JSON["next_action"].(map[string]any)["challenge_id"].(string)
	d2 := env.Must(env.Do("POST", "/v1/payment_intents/"+pi2.Str("id")+"/3ds", m.Key, map[string]any{"challenge_id": c2, "outcome": "success"}), http.StatusOK)
	if d2.Str("status") != "declined" || d2.Str("decline_code") != "do_not_honor" {
		t.Fatalf("3ds then decline: %s", d2.Raw)
	}

	// Cardholder fails the challenge.
	pi3 := createIntent(env, m.Key, 4200, nil)
	a3 := env.Must(env.Do("POST", "/v1/payment_intents/"+pi3.Str("id")+"/authorize", m.Key, testenv.Card("4000000000003220")), http.StatusOK)
	c3 := a3.JSON["next_action"].(map[string]any)["challenge_id"].(string)
	d3 := env.Must(env.Do("POST", "/v1/payment_intents/"+pi3.Str("id")+"/3ds", m.Key, map[string]any{"challenge_id": c3, "outcome": "failure"}), http.StatusOK)
	if d3.Str("decline_code") != "authentication_failed" {
		t.Fatalf("3ds failure: %s", d3.Raw)
	}
}

func TestDeclinesAndRetry(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Decline Shop")
	cases := map[string]string{
		"4000000000000002": "do_not_honor",
		"4000000000009995": "insufficient_funds",
		"4000000000009987": "lost_card",
		"4000000000009979": "stolen_card",
		"4000000000000069": "expired_card",
		"4000000000000127": "incorrect_cvc",
		"4000000000000119": "processing_error",
	}
	for pan, code := range cases {
		pi := createIntent(env, m.Key, 1000, nil)
		r := env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/authorize", m.Key, testenv.Card(pan)), http.StatusOK)
		if r.Str("status") != "declined" || r.Str("decline_code") != code {
			t.Errorf("%s: status=%s code=%s", pan, r.Str("status"), r.Str("decline_code"))
		}
	}
	// A declined intent can be retried with another card.
	pi := createIntent(env, m.Key, 1000, nil)
	env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/authorize", m.Key, testenv.Card("4000000000009995")), http.StatusOK)
	retry := env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/authorize", m.Key, testenv.Card("4242424242424242")), http.StatusOK)
	if retry.Str("status") != "authorized" || retry.Str("decline_code") != "" {
		t.Fatalf("retry: %s", retry.Raw)
	}
	// Authorizing twice is an invalid transition.
	again := env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/authorize", m.Key, testenv.Card("4242424242424242"))
	if again.Status != http.StatusConflict || again.ErrType() != "invalid_state_transition" {
		t.Fatalf("double authorize: %d %s", again.Status, again.Raw)
	}
}

func TestFraudBlockAndManualReview(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Risky Shop")
	admin := testenv.AdminToken

	// BIN 410000 is on the seeded blocklist.
	pi := createIntent(env, m.Key, 1000, nil)
	b := env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/authorize", m.Key, testenv.Card("4100000000000019")), http.StatusOK)
	if b.Str("status") != "blocked" {
		t.Fatalf("expected blocked: %s", b.Raw)
	}

	// A large amount triggers review; an operator approves it.
	big := createIntent(env, m.Key, 600_000, nil)
	r := env.Must(env.Do("POST", "/v1/payment_intents/"+big.Str("id")+"/authorize", m.Key, testenv.Card("4242424242424242")), http.StatusOK)
	if r.Str("status") != "requires_review" {
		t.Fatalf("expected review: %s", r.Raw)
	}
	// Merchants cannot capture while in review.
	if c := env.Do("POST", "/v1/payment_intents/"+big.Str("id")+"/capture", m.Key, nil); c.Status != http.StatusConflict {
		t.Fatalf("capture during review: %d", c.Status)
	}
	// Merchant keys cannot reach the admin API.
	if c := env.Do("POST", "/admin/v1/payment_intents/"+big.Str("id")+"/review", m.Key, map[string]any{"decision": "approve"}); c.Status != http.StatusUnauthorized {
		t.Fatalf("merchant on admin api: %d", c.Status)
	}
	ok := env.Must(env.Do("POST", "/admin/v1/payment_intents/"+big.Str("id")+"/review", admin, map[string]any{"decision": "approve", "note": "known customer"}), http.StatusOK)
	if ok.Str("status") != "authorized" {
		t.Fatalf("after approve: %s", ok.Raw)
	}

	// Another large one is rejected.
	big2 := createIntent(env, m.Key, 700_000, nil)
	env.Must(env.Do("POST", "/v1/payment_intents/"+big2.Str("id")+"/authorize", m.Key, testenv.Card("5555555555554444")), http.StatusOK)
	rej := env.Must(env.Do("POST", "/admin/v1/payment_intents/"+big2.Str("id")+"/review", admin, map[string]any{"decision": "reject"}), http.StatusOK)
	if rej.Str("status") != "blocked" {
		t.Fatalf("after reject: %s", rej.Raw)
	}

	// Admin can manage the blocklist; the blocked BIN then passes.
	env.Must(env.Do("DELETE", "/admin/v1/fraud/blocklist/410000", admin, nil), http.StatusNoContent)
	pi3 := createIntent(env, m.Key, 1000, nil)
	ok3 := env.Must(env.Do("POST", "/v1/payment_intents/"+pi3.Str("id")+"/authorize", m.Key, testenv.Card("4100000000000019")), http.StatusOK)
	if ok3.Str("status") != "authorized" {
		t.Fatalf("unblocked bin: %s", ok3.Raw)
	}
	env.Must(env.Do("POST", "/admin/v1/fraud/blocklist", admin, map[string]any{"bin": "555555", "reason": "test"}), http.StatusCreated)
	pi4 := createIntent(env, m.Key, 1000, nil)
	b4 := env.Must(env.Do("POST", "/v1/payment_intents/"+pi4.Str("id")+"/authorize", m.Key, testenv.Card("5555555555554444")), http.StatusOK)
	if b4.Str("status") != "blocked" {
		t.Fatalf("newly blocked bin: %s", b4.Raw)
	}
}

func TestCardVelocityBlocks(t *testing.T) {
	env := testenv.New(t, func(c *config.Config) { c.Fraud.CardVelocityMax = 2 })
	m := env.Onboard("Velocity Shop")
	var statuses []string
	for i := 0; i < 6; i++ {
		pi := createIntent(env, m.Key, 100, nil)
		r := env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/authorize", m.Key, testenv.Card("6011111111111117")), http.StatusOK)
		statuses = append(statuses, r.Str("status"))
	}
	// 1-2 allowed, 3-4 over the limit (score 40 -> allow), 5+ over 2x limit -> block.
	if statuses[0] != "authorized" || statuses[1] != "authorized" || statuses[5] != "blocked" {
		t.Fatalf("velocity statuses: %v", statuses)
	}
	// After the window passes, the card is clean again.
	env.Clock.Advance(11 * time.Minute)
	pi := createIntent(env, m.Key, 100, nil)
	r := env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/authorize", m.Key, testenv.Card("6011111111111117")), http.StatusOK)
	if r.Str("status") != "authorized" {
		t.Fatalf("after window: %s", r.Raw)
	}
}

func TestVoidCancelAndExpiry(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Void Shop")
	pi := createIntent(env, m.Key, 3000, map[string]any{"confirm": true, "payment_method": testenv.Card("378282246310005")})
	v := env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/void", m.Key, map[string]any{"reason": "duplicate"}), http.StatusOK)
	if v.Str("status") != "voided" || v.Str("cancellation_reason") != "duplicate" {
		t.Fatalf("void: %s", v.Raw)
	}
	if c := env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/capture", m.Key, nil); c.Status != http.StatusConflict {
		t.Fatalf("capture after void: %d", c.Status)
	}
	created := createIntent(env, m.Key, 3000, nil)
	c := env.Must(env.Do("POST", "/v1/payment_intents/"+created.Str("id")+"/cancel", m.Key, nil), http.StatusOK)
	if c.Str("status") != "canceled" {
		t.Fatalf("cancel: %s", c.Raw)
	}
	if bad := env.Do("POST", "/v1/payment_intents/"+created.Str("id")+"/void", m.Key, map[string]any{"reason": "bogus"}); bad.Status != http.StatusUnprocessableEntity {
		t.Fatalf("bad reason: %d", bad.Status)
	}

	// Authorization expiry job voids stale holds.
	held := createIntent(env, m.Key, 3000, map[string]any{"confirm": true, "payment_method": testenv.Card("3566002020360505")})
	env.Clock.Advance(8 * 24 * time.Hour)
	job := env.Must(env.Do("POST", "/admin/v1/jobs/expire_authorizations/run", testenv.AdminToken, nil), http.StatusOK)
	if job.JSON["result"].(map[string]any)["expired"].(float64) != 1 {
		t.Fatalf("expiry job: %s", job.Raw)
	}
	exp := env.Must(env.Do("GET", "/v1/payment_intents/"+held.Str("id"), m.Key, nil), http.StatusOK)
	if exp.Str("status") != "voided" || exp.Str("cancellation_reason") != "authorization_expired" {
		t.Fatalf("expired: %s", exp.Raw)
	}
}

func TestRefundFailureIsRecorded(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Refund Fail Shop")
	pi := createIntent(env, m.Key, 2000, map[string]any{"confirm": true, "capture_method": "automatic",
		"payment_method": testenv.Card("4000000000005126")})
	r := env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/refunds", m.Key, map[string]any{"amount": 500}), http.StatusCreated)
	if r.Str("status") != "failed" || r.Str("failure_code") != "refund_failed" {
		t.Fatalf("refund: %s", r.Raw)
	}
	p := env.Must(env.Do("GET", "/v1/payment_intents/"+pi.Str("id"), m.Key, nil), http.StatusOK)
	if p.Str("status") != "captured" || p.Num("amount_refunded") != 0 {
		t.Fatalf("payment changed by failed refund: %s", p.Raw)
	}
	list := env.Must(env.Do("GET", "/v1/payment_intents/"+pi.Str("id")+"/refunds", m.Key, nil), http.StatusOK)
	if len(list.JSON["data"].([]any)) != 1 {
		t.Fatalf("refund list: %s", list.Raw)
	}
}

func TestReconciliationFindsDiscrepancies(t *testing.T) {
	env := testenv.New(t, func(c *config.Config) {
		c.Acquirer.DiscrepancyRate = 1 // perturb every row
		c.Acquirer.DiscrepancySeed = "integration"
	})
	m := env.Onboard("Recon Shop")
	for i := 0; i < 8; i++ {
		createIntent(env, m.Key, int64(1000+i), map[string]any{"confirm": true, "capture_method": "automatic",
			"payment_method": testenv.Card("4242424242424242")})
	}
	day := env.Clock.Now().Format(clock.DateLayout)
	run := env.Must(env.Do("POST", "/admin/v1/reconciliations", testenv.AdminToken, map[string]any{"date": day}), http.StatusCreated)
	if run.Str("status") != "discrepancies" || run.Num("discrepancy_count") == 0 {
		t.Fatalf("expected discrepancies: %s", run.Raw)
	}
	got := env.Must(env.Do("GET", "/admin/v1/reconciliations/"+run.Str("id"), testenv.AdminToken, nil), http.StatusOK)
	if len(got.JSON["discrepancies"].([]any)) != int(run.Num("discrepancy_count")) {
		t.Fatalf("stored discrepancies mismatch: %s", got.Raw)
	}

	// Upload a hand-written file: one matching capture is missing, one unknown row added.
	file := env.Must(env.Do("GET", "/admin/v1/acquirer/settlement_file?date="+day, testenv.AdminToken, nil), http.StatusOK)
	if !strings.HasPrefix(string(file.Raw), "record_type,acquirer_ref") {
		t.Fatalf("settlement file: %s", file.Raw)
	}
	upload := "record_type,acquirer_ref,parent_ref,merchant_id,amount,currency,transaction_time\n" +
		"CAPTURE,acq_not_ours,,mer_x,100,USD,2026-03-02T10:00:00Z\n"
	req := env.Do("POST", "/admin/v1/reconciliations/upload?date="+day, testenv.AdminToken, upload, "Content-Type", "text/csv")
	if req.Status != http.StatusCreated || req.Num("matched_count") != 0 || req.Num("discrepancy_count") != 9 {
		t.Fatalf("upload reconciliation: %d %s", req.Status, req.Raw)
	}
	// A file containing a PAN is rejected outright.
	panFile := "record_type,acquirer_ref,parent_ref,merchant_id,amount,currency,transaction_time\n" +
		"CAPTURE,4242424242424242,,mer_x,100,USD,2026-03-02T10:00:00Z\n"
	rej := env.Do("POST", "/admin/v1/reconciliations/upload?date="+day, testenv.AdminToken, panFile, "Content-Type", "text/csv")
	if rej.Status != http.StatusUnprocessableEntity || strings.Contains(string(rej.Raw), "4242424242424242") {
		t.Fatalf("pan file: %d %s", rej.Status, rej.Raw)
	}
}
