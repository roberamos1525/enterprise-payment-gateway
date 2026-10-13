package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

func TestAuthenticationAndIsolation(t *testing.T) {
	env := testenv.New(t)
	a := env.Onboard("Alpha")
	b := env.Onboard("Beta")

	if r := env.Do("GET", "/v1/merchant", "", nil); r.Status != http.StatusUnauthorized || r.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("missing key: %d", r.Status)
	}
	if r := env.Do("GET", "/v1/merchant", "sk_test_abcdefgh_"+strings.Repeat("x", 32), nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("unknown key: %d", r.Status)
	}
	// A valid prefix with a wrong secret is rejected too.
	forged := a.Key[:len(a.Key)-1] + "z"
	if strings.HasSuffix(a.Key, "z") {
		forged = a.Key[:len(a.Key)-1] + "y"
	}
	if r := env.Do("GET", "/v1/merchant", forged, nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("forged secret: %d", r.Status)
	}
	if r := env.Do("GET", "/admin/v1/merchants", "wrong-token-wrong-token-wrong", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("bad admin token: %d", r.Status)
	}
	if r := env.Do("GET", "/metrics", "", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("metrics should require admin: %d", r.Status)
	}
	metrics := env.Must(env.Do("GET", "/metrics", testenv.AdminToken, nil), http.StatusOK)
	if !strings.Contains(string(metrics.Raw), "gateway_http_requests_total") {
		t.Fatalf("metrics body: %s", metrics.Raw)
	}

	// Merchant B cannot see or act on merchant A's payment.
	pi := createIntent(env, a.Key, 1000, nil)
	if r := env.Do("GET", "/v1/payment_intents/"+pi.Str("id"), b.Key, nil); r.Status != http.StatusNotFound {
		t.Fatalf("cross-merchant read: %d", r.Status)
	}
	if r := env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/cancel", b.Key, nil); r.Status != http.StatusNotFound {
		t.Fatalf("cross-merchant cancel: %d", r.Status)
	}
	// Tokens are merchant-scoped as well.
	tok := env.Must(env.Do("POST", "/v1/tokens", a.Key, map[string]any{"card": testenv.Card("4242424242424242")["card"]}), http.StatusCreated)
	pb := createIntent(env, b.Key, 1000, nil)
	if r := env.Do("POST", "/v1/payment_intents/"+pb.Str("id")+"/authorize", b.Key, map[string]any{"token": tok.Str("token")}); r.Status != http.StatusNotFound {
		t.Fatalf("cross-merchant token: %d %s", r.Status, r.Raw)
	}
	list := env.Must(env.Do("GET", "/v1/payment_intents", b.Key, nil), http.StatusOK)
	if len(list.JSON["data"].([]any)) != 1 {
		t.Fatalf("merchant B should only see its own payment: %s", list.Raw)
	}
}

func TestKeyRotationAndMerchantLifecycle(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Rotator")
	admin := testenv.AdminToken

	// Self-rotation with a one-hour grace period.
	rot := env.Must(env.Do("POST", "/v1/api_keys/rotate", m.Key, map[string]any{"grace_period_seconds": 3600}), http.StatusCreated)
	newKey := rot.Str("secret")
	env.Must(env.Do("GET", "/v1/merchant", m.Key, nil), http.StatusOK) // old key still valid in grace
	env.Must(env.Do("GET", "/v1/merchant", newKey, nil), http.StatusOK)
	env.Clock.Advance(61 * time.Minute)
	if r := env.Do("GET", "/v1/merchant", m.Key, nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("old key after grace: %d", r.Status)
	}
	env.Must(env.Do("GET", "/v1/merchant", newKey, nil), http.StatusOK)

	// Admin revokes the new key.
	keys := env.Must(env.Do("GET", "/admin/v1/merchants/"+m.ID+"/api_keys", admin, nil), http.StatusOK)
	if strings.Contains(string(keys.Raw), "hash") || strings.Contains(string(keys.Raw), newKey) {
		t.Fatalf("key listing exposes secrets: %s", keys.Raw)
	}
	env.Must(env.Do("DELETE", "/admin/v1/merchants/"+m.ID+"/api_keys/"+rot.Str("id"), admin, nil), http.StatusNoContent)
	if r := env.Do("GET", "/v1/merchant", newKey, nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d", r.Status)
	}

	// Suspended merchants cannot create payments but can still refund.
	k2 := env.Must(env.Do("POST", "/admin/v1/merchants/"+m.ID+"/api_keys", admin, map[string]any{"label": "k2"}), http.StatusCreated).Str("secret")
	pi := createIntent(env, k2, 1000, map[string]any{"confirm": true, "capture_method": "automatic", "payment_method": testenv.Card("4242424242424242")})
	env.Must(env.Do("POST", "/admin/v1/merchants/"+m.ID+"/status", admin, map[string]any{"status": "suspended", "reason": "kyc"}), http.StatusOK)
	if r := env.Do("POST", "/v1/payment_intents", k2, map[string]any{"amount": 100, "currency": "USD"}); r.Status != http.StatusForbidden {
		t.Fatalf("suspended create: %d", r.Status)
	}
	env.Must(env.Do("POST", "/v1/payment_intents/"+pi.Str("id")+"/refunds", k2, nil), http.StatusCreated)
	// Invalid lifecycle move.
	if r := env.Do("POST", "/admin/v1/merchants/"+m.ID+"/status", admin, map[string]any{"status": "pending"}); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("to pending: %d", r.Status)
	}
	// Closing revokes every key.
	env.Must(env.Do("POST", "/admin/v1/merchants/"+m.ID+"/status", admin, map[string]any{"status": "closed"}), http.StatusOK)
	if r := env.Do("GET", "/v1/merchant", k2, nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("closed merchant key: %d", r.Status)
	}
	if r := env.Do("POST", "/admin/v1/merchants/"+m.ID+"/status", admin, map[string]any{"status": "active"}); r.Status != http.StatusConflict {
		t.Fatalf("reopen closed: %d", r.Status)
	}
}

func TestRateLimiting(t *testing.T) {
	env := testenv.New(t, func(c *config.Config) { c.RateLimit.MerchantRPS, c.RateLimit.MerchantBurst = 1, 3 })
	m := env.Onboard("Busy")
	var last testenv.Response
	for i := 0; i < 4; i++ {
		last = env.Do("GET", "/v1/merchant", m.Key, nil)
	}
	if last.Status != http.StatusTooManyRequests || last.ErrType() != "rate_limited" {
		t.Fatalf("expected 429, got %d", last.Status)
	}
	if last.Header.Get("Retry-After") == "" || last.Header.Get("X-RateLimit-Limit") != "3" {
		t.Fatalf("rate limit headers missing: %v", last.Header)
	}
}

func TestErrorEnvelopes(t *testing.T) {
	env := testenv.New(t, func(c *config.Config) { c.MaxBodyBytes = 2048 })
	m := env.Onboard("Errors")
	cases := []struct {
		name, method, path string
		body               any
		headers            []string
		status             int
		errType            string
	}{
		{"unknown route", "GET", "/v1/nope", nil, nil, 404, "not_found"},
		{"wrong method", "DELETE", "/v1/merchant", nil, nil, 405, "method_not_allowed"},
		{"malformed json", "POST", "/v1/payment_intents", "{bad", nil, 400, "invalid_request"},
		{"unknown field", "POST", "/v1/payment_intents", map[string]any{"amount": 1, "currency": "USD", "colour": "red"}, nil, 400, "invalid_request"},
		{"wrong type", "POST", "/v1/payment_intents", map[string]any{"amount": "ten", "currency": "USD"}, nil, 422, "validation_failed"},
		{"validation", "POST", "/v1/payment_intents", map[string]any{"amount": 0, "currency": "XXX"}, nil, 422, "validation_failed"},
		{"too large", "POST", "/v1/payment_intents", map[string]any{"description": strings.Repeat("a", 4000)}, nil, 413, "payload_too_large"},
		{"bad content type", "POST", "/v1/payment_intents", `{"amount":1}`, []string{"Content-Type", "text/plain"}, 415, "unsupported_media_type"},
		{"bad cursor limit", "GET", "/v1/payment_intents?limit=500", nil, nil, 422, "validation_failed"},
		{"missing payment", "GET", "/v1/payment_intents/pi_missing", nil, nil, 404, "not_found"},
	}
	for _, c := range cases {
		r := env.Do(c.method, c.path, m.Key, c.body, c.headers...)
		if r.Status != c.status || r.ErrType() != c.errType {
			t.Errorf("%s: got %d %q, want %d %q (%s)", c.name, r.Status, r.ErrType(), c.status, c.errType, r.Raw)
		}
		if r.JSON != nil {
			e := r.JSON["error"].(map[string]any)
			if e["request_id"] == "" || e["message"] == "" {
				t.Errorf("%s: incomplete envelope %s", c.name, r.Raw)
			}
		}
	}
	v := env.Do("POST", "/v1/payment_intents", m.Key, map[string]any{"amount": 0, "currency": "XXX", "capture_method": "later"})
	fields := v.JSON["error"].(map[string]any)["fields"].([]any)
	if len(fields) != 3 {
		t.Fatalf("expected all three field errors at once: %s", v.Raw)
	}
	pub := env.Must(env.Do("GET", "/v1/test_cards", "", nil), http.StatusOK)
	if pub.Num("total") < 20 {
		t.Fatalf("test cards: %s", pub.Raw)
	}
}

func TestPaginationOverHTTP(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Pages")
	for i := 0; i < 7; i++ {
		createIntent(env, m.Key, int64(100+i), nil)
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/v1/payment_intents?limit=3"
		if cursor != "" {
			path += "&starting_after=" + cursor
		}
		r := env.Must(env.Do("GET", path, m.Key, nil), http.StatusOK)
		pages++
		for _, item := range r.JSON["data"].([]any) {
			id := item.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("duplicate %s across pages", id)
			}
			seen[id] = true
		}
		if r.JSON["has_more"] != true {
			break
		}
		cursor = r.Str("next_cursor")
	}
	if len(seen) != 7 || pages != 3 {
		t.Fatalf("saw %d items over %d pages", len(seen), pages)
	}
	f := env.Must(env.Do("GET", "/v1/payment_intents?status=created&limit=100", m.Key, nil), http.StatusOK)
	if len(f.JSON["data"].([]any)) != 7 {
		t.Fatalf("status filter: %s", f.Raw)
	}
	if r := env.Do("GET", "/v1/payment_intents?status=bogus", m.Key, nil); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("bad status filter: %d", r.Status)
	}
}

func TestWebhookEndpointURLPolicy(t *testing.T) {
	env := testenv.New(t, func(c *config.Config) {
		c.Webhooks.AllowInsecureHTTP, c.Webhooks.AllowPrivateNetworks = false, false
	})
	m := env.Onboard("Strict Hooks")
	for _, u := range []string{"http://example.com/h", "https://127.0.0.1/h", "https://localhost/h",
		"https://user:pw@example.com/h", "ftp://example.com", "https://10.1.2.3/h", "https://[::1]/h"} {
		r := env.Do("POST", "/v1/webhook_endpoints", m.Key, map[string]any{"url": u, "events": []string{"*"}})
		if r.Status != http.StatusUnprocessableEntity {
			t.Errorf("%s accepted (%d)", u, r.Status)
		}
	}
	env.Must(env.Do("POST", "/v1/webhook_endpoints", m.Key, map[string]any{"url": "https://hooks.example.com/x",
		"events": []string{"payment_intent.captured"}}), http.StatusCreated)
	if r := env.Do("POST", "/v1/webhook_endpoints", m.Key, map[string]any{"url": "https://hooks.example.com/y",
		"events": []string{"payment_intent.exploded"}}); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown event accepted: %d", r.Status)
	}
}
