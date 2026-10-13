package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

// TestSecondaryEndpoints exercises read/update endpoints not covered by the
// main flows: merchant profile and fee schedules, key listing, refunds and
// events by id, settlement filters, payouts and the admin payment view.
func TestSecondaryEndpoints(t *testing.T) {
	env := testenv.New(t)
	admin := testenv.AdminToken
	m := env.Onboard("Secondary")

	up := env.Must(env.Do("PATCH", "/admin/v1/merchants/"+m.ID, admin, map[string]any{"name": "Secondary Ltd"}), http.StatusOK)
	if up.Str("name") != "Secondary Ltd" {
		t.Fatalf("profile update: %s", up.Raw)
	}
	other := env.Onboard("Taken")
	if r := env.Do("PATCH", "/admin/v1/merchants/"+m.ID, admin, map[string]any{"email": other.Email}); r.Status != http.StatusConflict {
		t.Fatalf("duplicate email update: %d", r.Status)
	}
	fee := env.Must(env.Do("POST", "/admin/v1/merchants/"+m.ID+"/fee_schedules", admin,
		map[string]any{"percent_bps": 100, "fixed_minor": 0}), http.StatusCreated)
	if fee.Num("percent_bps") != 100 {
		t.Fatalf("fee schedule: %s", fee.Raw)
	}
	if r := env.Do("POST", "/admin/v1/merchants/"+m.ID+"/fee_schedules", admin, map[string]any{"percent_bps": -1}); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("negative fee: %d", r.Status)
	}
	keys := env.Must(env.Do("GET", "/v1/api_keys", m.Key, nil), http.StatusOK)
	if len(keys.JSON["data"].([]any)) != 1 {
		t.Fatalf("keys: %s", keys.Raw)
	}

	pi := createIntent(env, m.Key, 10_000, map[string]any{"confirm": true, "capture_method": "automatic",
		"payment_method": testenv.Card("2223003122003222")})
	ref := env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/refunds", m.Key, map[string]any{"amount": 100}), http.StatusCreated)
	got := env.Must(env.Do("GET", "/v1/refunds/"+ref.Str("id"), m.Key, nil), http.StatusOK)
	if got.Num("amount") != 100 || got.Str("payment_intent_id") != pi.Str("id") {
		t.Fatalf("refund by id: %s", got.Raw)
	}
	if r := env.Do("GET", "/v1/refunds/"+ref.Str("id"), other.Key, nil); r.Status != http.StatusNotFound {
		t.Fatalf("foreign refund: %d", r.Status)
	}
	adminView := env.Must(env.Do("GET", "/admin/v1/payment_intents/"+pi.Str("id"), admin, nil), http.StatusOK)
	if adminView.Str("status") != "partially_refunded" || adminView.JSON["card"].(map[string]any)["brand"] != "mastercard" {
		t.Fatalf("admin payment view: %s", adminView.Raw)
	}

	events := env.Must(env.Do("GET", "/v1/events?resource_id="+pi.Str("id"), m.Key, nil), http.StatusOK)
	evList := events.JSON["data"].([]any)
	if len(evList) != 3 { // created, authorized, captured
		t.Fatalf("events for payment: %s", events.Raw)
	}
	evID := evList[0].(map[string]any)["id"].(string)
	env.Must(env.Do("GET", "/v1/events/"+evID, m.Key, nil), http.StatusOK)
	if r := env.Do("GET", "/v1/events/"+evID, other.Key, nil); r.Status != http.StatusNotFound {
		t.Fatalf("foreign event: %d", r.Status)
	}

	day := env.Clock.Now().Format(clock.DateLayout)
	env.Clock.Advance(15 * time.Hour)
	env.Must(env.Do("POST", "/admin/v1/settlements/run", admin, map[string]any{"date": day, "merchant_id": m.ID}), http.StatusOK)
	list := env.Must(env.Do("GET", "/v1/settlements?date="+day, m.Key, nil), http.StatusOK)
	batches := list.JSON["data"].([]any)
	if len(batches) != 1 {
		t.Fatalf("settlements: %s", list.Raw)
	}
	b := batches[0].(map[string]any)
	// New schedule (1%, no fixed fee): 10000 -> 100 fee; refund fee 0.
	if b["fees"].(float64) != 100 || b["net"].(float64) != 9_800 {
		t.Fatalf("batch: %v", b)
	}
	one := env.Must(env.Do("GET", "/v1/settlements/"+b["id"].(string), m.Key, nil), http.StatusOK)
	if one.Str("payout_id") == "" {
		t.Fatalf("batch by id: %s", one.Raw)
	}
	if r := env.Do("GET", "/v1/settlements?date=03-02-2026", m.Key, nil); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("bad date filter: %d", r.Status)
	}
	adminCSV := env.Must(env.Do("GET", "/admin/v1/settlements/"+b["id"].(string)+"/report?format=csv", admin, nil), http.StatusOK)
	if !strings.HasPrefix(adminCSV.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("csv content type: %s", adminCSV.Header.Get("Content-Type"))
	}
	payouts := env.Must(env.Do("GET", "/v1/payouts", m.Key, nil), http.StatusOK)
	po := payouts.JSON["data"].([]any)
	if len(po) != 1 || po[0].(map[string]any)["amount"].(float64) != 9_800 {
		t.Fatalf("payouts: %s", payouts.Raw)
	}
	adminList := env.Must(env.Do("GET", "/admin/v1/settlements?merchant_id="+m.ID, admin, nil), http.StatusOK)
	if len(adminList.JSON["data"].([]any)) != 1 {
		t.Fatalf("admin settlements: %s", adminList.Raw)
	}
}
