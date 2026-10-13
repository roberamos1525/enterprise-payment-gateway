package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

func TestRequestIDAndSecurityHeaders(t *testing.T) {
	env := testenv.New(t)
	r := env.Do("GET", "/healthz", "", nil, "X-Request-Id", "req-from-client")
	if r.Header.Get("X-Request-Id") != "req-from-client" {
		t.Fatalf("client request id not propagated: %q", r.Header.Get("X-Request-Id"))
	}
	for h, want := range map[string]string{
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Cache-Control": "no-store",
		"Referrer-Policy": "no-referrer",
	} {
		if got := r.Header.Get(h); got != want {
			t.Errorf("%s = %q", h, got)
		}
	}
	// Hostile request ids are replaced.
	r = env.Do("GET", "/healthz", "", nil, "X-Request-Id", "has spaces in it")
	if strings.Contains(r.Header.Get("X-Request-Id"), " ") || !strings.HasPrefix(r.Header.Get("X-Request-Id"), "req_") {
		t.Fatalf("request id = %q", r.Header.Get("X-Request-Id"))
	}
	ready := env.Must(env.Do("GET", "/readyz", "", nil), http.StatusOK)
	if ready.Str("status") != "ready" {
		t.Fatalf("readyz = %s", ready.Raw)
	}
}

func TestAccessLogRecordsRouteAndActor(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Logged")
	env.Must(env.Do("GET", "/v1/merchant", m.Key, nil), http.StatusOK)
	logs := env.Logs.String()
	if !strings.Contains(logs, `"route":"GET /v1/merchant"`) || !strings.Contains(logs, `"actor":"merchant:key_`) {
		t.Fatalf("access log missing route/actor: %s", logs)
	}
	if strings.Contains(logs, m.Key) {
		t.Fatal("API key secret written to logs")
	}
}

func TestForwardedForOnlyWhenTrusted(t *testing.T) {
	ip := func(trust bool) string {
		env := testenv.New(t, func(c *config.Config) { c.TrustProxyHeaders = trust })
		m := env.Onboard("Proxy")
		pi := env.Must(env.Do("POST", "/v1/payment_intents", m.Key, map[string]any{"amount": 100, "currency": "USD"}), http.StatusCreated)
		env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/authorize", m.Key, testenv.Card("4242424242424242"),
			"X-Forwarded-For", "203.0.113.50, 10.0.0.1"), http.StatusOK)
		entries := env.Must(env.Do("GET", "/admin/v1/audit_logs?action=payment.authorize", testenv.AdminToken, nil), http.StatusOK)
		data := entries.JSON["data"].([]any)
		return data[0].(map[string]any)["ip"].(string)
	}
	if got := ip(true); got != "203.0.113.50" {
		t.Fatalf("trusted proxy ip = %q", got)
	}
	if got := ip(false); got == "203.0.113.50" {
		t.Fatal("X-Forwarded-For must be ignored without a trusted proxy")
	}
}

func TestAdminReadEndpoints(t *testing.T) {
	env := testenv.New(t)
	admin := testenv.AdminToken
	m := env.Onboard("Admin Reads")
	env.Must(env.Do("POST", "/v1/payment_intents", m.Key, map[string]any{"amount": 100, "currency": "USD",
		"confirm": true, "payment_method": testenv.Card("4242424242424242")}), http.StatusCreated)
	checks := map[string]string{
		"/admin/v1/merchants/" + m.ID:                         `"status":"active"`,
		"/admin/v1/merchants?status=active":                   m.ID,
		"/admin/v1/merchants/" + m.ID + "/fee_schedules":      `"percent_bps":290`,
		"/admin/v1/merchants/" + m.ID + "/balance":            `"object":"balance"`,
		"/admin/v1/payment_intents?merchant_id=" + m.ID:       `"status":"authorized"`,
		"/admin/v1/fraud/rules":                               `"bin_blocklist"`,
		"/admin/v1/fraud/blocklist":                           `"410000"`,
		"/admin/v1/vault/status":                              `"primary_key":"k1"`,
		"/admin/v1/settlements/summary?date=2026-03-02":       `"currencies":[]`,
		"/admin/v1/reconciliations":                           `"object":"list"`,
		"/admin/v1/audit_logs?resource_type=merchant&limit=5": `"merchant.onboard"`,
	}
	for path, want := range checks {
		r := env.Must(env.Do("GET", path, admin, nil), http.StatusOK)
		if !strings.Contains(string(r.Raw), want) {
			t.Errorf("%s: missing %s in %s", path, want, r.Raw)
		}
	}
	for path, status := range map[string]int{
		"/admin/v1/merchants?status=zombie":      http.StatusUnprocessableEntity,
		"/admin/v1/merchants/mer_missing":        http.StatusNotFound,
		"/admin/v1/settlements/summary?date=bad": http.StatusUnprocessableEntity,
		"/admin/v1/audit_logs?after_seq=-1":      http.StatusUnprocessableEntity,
		"/admin/v1/acquirer/settlement_file":     http.StatusUnprocessableEntity,
		"/admin/v1/reconciliations/rec_missing":  http.StatusNotFound,
	} {
		if r := env.Do("GET", path, admin, nil); r.Status != status {
			t.Errorf("%s: %d, want %d", path, r.Status, status)
		}
	}
	rew := env.Must(env.Do("POST", "/admin/v1/vault/rewrap", admin, nil), http.StatusOK)
	if rew.Num("rewrapped") != 0 {
		t.Fatalf("rewrap with a single key: %s", rew.Raw)
	}
}
