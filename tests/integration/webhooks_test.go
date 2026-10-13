package integration

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

func TestWebhookRetriesBackoffAndDeliveryLog(t *testing.T) {
	env := testenv.New(t, func(c *config.Config) { c.Webhooks.MaxAttempts = 3 })
	m := env.Onboard("Hooks")
	rc := newReceiver(t, env.Clock.Now)
	rc.failures = 1
	ep := env.Must(env.Do("POST", "/v1/webhook_endpoints", m.Key, map[string]any{
		"url": rc.srv.URL, "events": []string{"payment_intent.created"}, "description": "orders",
	}), http.StatusCreated)
	rc.secret = ep.Str("secret")
	createIntent(env, m.Key, 100, nil)

	if n := env.DispatchAll(); n != 1 {
		t.Fatalf("first pass attempted %d", n)
	}
	// The retry is scheduled in the future: nothing is due yet.
	if n := env.DispatchAll(); n != 0 {
		t.Fatalf("retry fired before backoff elapsed (%d)", n)
	}
	env.Clock.Advance(time.Minute)
	if n := env.DispatchAll(); n != 1 {
		t.Fatalf("retry pass attempted %d", n)
	}
	if got := rc.types(); len(got) != 1 || got[0] != "payment_intent.created" {
		t.Fatalf("receiver got %v", got)
	}
	deliveries := env.Must(env.Do("GET", "/v1/webhook_endpoints/"+ep.Str("id")+"/deliveries", m.Key, nil), http.StatusOK)
	data := deliveries.JSON["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("deliveries: %s", deliveries.Raw)
	}
	d := data[0].(map[string]any)
	if d["status"] != "succeeded" || d["attempts"].(float64) != 2 {
		t.Fatalf("delivery: %v", d)
	}
	detail := env.Must(env.Do("GET", "/v1/webhook_deliveries/"+d["id"].(string), m.Key, nil), http.StatusOK)
	log := detail.JSON["attempt_log"].([]any)
	if len(log) != 2 || log[0].(map[string]any)["status_code"].(float64) != 500 {
		t.Fatalf("attempt log: %s", detail.Raw)
	}

	// An endpoint that always fails exhausts its attempts.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	env.Must(env.Do("PATCH", "/v1/webhook_endpoints/"+ep.Str("id"), m.Key, map[string]any{"url": down.URL}), http.StatusOK)
	createIntent(env, m.Key, 200, nil)
	for i := 0; i < 5; i++ {
		env.DispatchAll()
		env.Clock.Advance(2 * time.Hour)
	}
	failed := env.Must(env.Do("GET", "/v1/webhook_endpoints/"+ep.Str("id")+"/deliveries?status=failed", m.Key, nil), http.StatusOK)
	fd := failed.JSON["data"].([]any)
	if len(fd) != 1 || fd[0].(map[string]any)["attempts"].(float64) != 3 {
		t.Fatalf("failed deliveries: %s", failed.Raw)
	}

	// Replay the failed event once the endpoint is healthy again.
	env.Must(env.Do("PATCH", "/v1/webhook_endpoints/"+ep.Str("id"), m.Key, map[string]any{"url": rc.srv.URL}), http.StatusOK)
	evID := fd[0].(map[string]any)["event_id"].(string)
	rep := env.Must(env.Do("POST", "/v1/events/"+evID+"/replay", m.Key, map[string]any{}), http.StatusAccepted)
	rd := rep.JSON["data"].([]any)[0].(map[string]any)
	if rd["replay_of"] != fd[0].(map[string]any)["id"] {
		t.Fatalf("replay link: %s", rep.Raw)
	}
	env.DispatchAll()
	if got := rc.types(); len(got) != 2 {
		t.Fatalf("after replay receiver got %v", got)
	}
}

func TestWebhookSecretRotationSignsWithBothSecrets(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Rotating Hooks")
	var mu sync.Mutex
	var headers []string
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		headers = append(headers, r.Header.Get(webhook.HeaderSignature))
		bodies = append(bodies, b)
		mu.Unlock()
	}))
	defer srv.Close()
	ep := env.Must(env.Do("POST", "/v1/webhook_endpoints", m.Key, map[string]any{"url": srv.URL, "events": []string{"*"}}), http.StatusCreated)
	oldSecret := ep.Str("secret")
	rot := env.Must(env.Do("POST", "/v1/webhook_endpoints/"+ep.Str("id")+"/rotate_secret", m.Key, nil), http.StatusOK)
	newSecret := rot.Str("secret")
	if newSecret == oldSecret || !strings.HasPrefix(newSecret, "whsec_") {
		t.Fatalf("rotation returned %q", newSecret)
	}
	if strings.Contains(string(env.Must(env.Do("GET", "/v1/webhook_endpoints/"+ep.Str("id"), m.Key, nil), http.StatusOK).Raw), "whsec_") {
		t.Fatal("endpoint read exposes the secret")
	}
	env.Must(env.Do("POST", "/v1/webhook_endpoints/"+ep.Str("id")+"/ping", m.Key, nil), http.StatusAccepted)
	env.DispatchAll()
	now := env.Clock.Now()
	if len(headers) != 1 {
		t.Fatalf("expected one ping, got %d", len(headers))
	}
	for _, s := range []string{oldSecret, newSecret} {
		if err := webhook.Verify(headers[0], bodies[0], s, time.Minute, now); err != nil {
			t.Fatalf("signature with %s... failed: %v", s[:10], err)
		}
	}
	// After the grace period only the new secret signs.
	env.Clock.Advance(25 * time.Hour)
	env.Must(env.Do("POST", "/v1/webhook_endpoints/"+ep.Str("id")+"/ping", m.Key, nil), http.StatusAccepted)
	env.DispatchAll()
	if err := webhook.Verify(headers[1], bodies[1], oldSecret, time.Minute, env.Clock.Now()); err == nil {
		t.Fatal("old secret still valid after grace period")
	}
	if err := webhook.Verify(headers[1], bodies[1], newSecret, time.Minute, env.Clock.Now()); err != nil {
		t.Fatalf("new secret: %v", err)
	}
	// Disabled endpoints receive nothing and cannot be pinged.
	env.Must(env.Do("PATCH", "/v1/webhook_endpoints/"+ep.Str("id"), m.Key, map[string]any{"enabled": false}), http.StatusOK)
	if r := env.Do("POST", "/v1/webhook_endpoints/"+ep.Str("id")+"/ping", m.Key, nil); r.Status != http.StatusConflict {
		t.Fatalf("ping disabled endpoint: %d", r.Status)
	}
	createIntent(env, m.Key, 100, nil)
	if n := env.DispatchAll(); n != 0 {
		t.Fatalf("disabled endpoint got %d deliveries", n)
	}
	events := env.Must(env.Do("GET", "/v1/events?type=payment_intent.created", m.Key, nil), http.StatusOK)
	if len(events.JSON["data"].([]any)) != 1 {
		t.Fatalf("event filter: %s", events.Raw)
	}
}
