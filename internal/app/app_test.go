package app_test

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/app"
	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/internal/logging"
	"github.com/robertramos/enterprise-payment-gateway/internal/payment"
	"github.com/robertramos/enterprise-payment-gateway/internal/settlement"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

func TestJobsAreRegistered(t *testing.T) {
	env := testenv.New(t)
	want := []string{"dispatch_webhooks", "expire_authorizations", "pay_payouts", "purge_idempotency", "settle_previous_day"}
	got := env.App.Jobs.Names()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("jobs = %v", got)
	}
}

func TestScheduledSettlementJob(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Scheduled")
	ctx := context.Background()
	if _, err := env.App.Payments.Create(ctx, m.Merchant, payment.CreateInput{Amount: 2_000, Currency: "USD",
		CaptureMethod: "automatic", Confirm: true, PaymentMethod: &payment.AuthorizeInput{Token: tokenFor(t, env, m)}}); err != nil {
		t.Fatal(err)
	}
	// Next day at 00:30 UTC: before the configured hour (01:00) nothing happens.
	env.Clock.Set(testenv.Start.Truncate(24 * time.Hour).Add(24*time.Hour + 30*time.Minute))
	res, err := env.App.Jobs.RunNow(ctx, "settle_previous_day")
	if err != nil || !strings.Contains(stringOf(res), "before configured settlement hour") {
		t.Fatalf("early run = %v %v", res, err)
	}
	env.Clock.Advance(time.Hour)
	res, err = env.App.Jobs.RunNow(ctx, "settle_previous_day")
	run, ok := res.(settlement.RunResult)
	if err != nil || !ok || len(run.Batches) != 1 || run.Date != "2026-03-02" {
		t.Fatalf("scheduled run = %+v %v", res, err)
	}
}

func tokenFor(t *testing.T, env *testenv.Env, m testenv.Merchant) string {
	t.Helper()
	r := env.Must(env.Do("POST", "/v1/tokens", m.Key, map[string]any{"card": testenv.Card("4242424242424242")["card"]}), http.StatusCreated)
	return r.Str("token")
}

func stringOf(v any) string {
	if m, ok := v.(map[string]string); ok {
		return m["skipped"]
	}
	return ""
}

func TestGaugesAndServeLifecycle(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Gauges")
	env.Must(env.Do("POST", "/v1/payment_intents", m.Key, map[string]any{"amount": 100, "currency": "USD"}), http.StatusCreated)
	metrics := env.Must(env.Do("GET", "/metrics", testenv.AdminToken, nil), http.StatusOK)
	if !strings.Contains(string(metrics.Raw), `gateway_payment_intents{status="created"} 1`) {
		t.Fatalf("gauge missing: %s", metrics.Raw)
	}

	// Serve on a real listener and shut down gracefully on cancel.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- env.App.Serve(ctx, ln) }()
	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("live healthz: %v", err)
	}
	resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestNewRejectsBadSecrets(t *testing.T) {
	base := testenv.Config(t.TempDir())
	for name, mutate := range map[string]func(*config.Config){
		"vault key":    func(c *config.Config) { c.VaultKeys = "k1:tooshort" },
		"admin tokens": func(c *config.Config) { c.AdminTokens = "ops:short" },
		"rounding":     func(c *config.Config) { c.Fees.Rounding = "sideways" },
		"db path":      func(c *config.Config) { c.DBPath = filepath.Join(t.TempDir(), "x?y.db") },
		"fraud":        func(c *config.Config) { c.Fraud.ReviewScore = 0 },
		"geo":          func(c *config.Config) { c.Fraud.GeoRanges = map[string]string{"nonsense": "US"} },
	} {
		cfg := base
		mutate(&cfg)
		if a, err := app.New(context.Background(), cfg, app.Options{Log: logging.Discard()}); err == nil {
			_ = a.Stop()
			t.Errorf("%s: app.New accepted bad configuration", name)
		}
	}
}
