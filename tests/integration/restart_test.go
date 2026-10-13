package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/robertramos/enterprise-payment-gateway/internal/app"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/internal/logging"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

// TestRestartAndVaultKeyRotation stops the gateway, restarts it on the same
// database with a new primary vault key, and checks that existing tokens
// still work, re-wrapping moves them to the new key and the audit chain
// continues unbroken.
func TestRestartAndVaultKeyRotation(t *testing.T) {
	dir := t.TempDir()
	cfg := testenv.Config(dir)
	clk := clock.NewFake(testenv.Start)
	start := func(c config.Config) (*app.App, *httptest.Server, *testenv.Env) {
		a, err := app.New(context.Background(), c, app.Options{Clock: clk, Log: logging.Discard()})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(a.Handler)
		return a, srv, &testenv.Env{T: t, App: a, Clock: clk, Server: srv}
	}

	a1, srv1, env1 := start(cfg)
	m := env1.Onboard("Persistent")
	tok := env1.Must(env1.Do("POST", "/v1/tokens", m.Key, map[string]any{"card": testenv.Card("4000056655665556")["card"]}), http.StatusCreated)
	srv1.Close()
	if err := a1.Stop(); err != nil {
		t.Fatal(err)
	}

	cfg2 := cfg
	cfg2.VaultKeys = "k2:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)) + "," + cfg.VaultKeys
	a2, srv2, env2 := start(cfg2)
	defer func() { srv2.Close(); _ = a2.Stop() }()

	// The merchant, key and token survived the restart.
	pi := env2.Must(env2.Do("POST", "/v1/payment_intents", m.Key, map[string]any{"amount": 777, "currency": "USD",
		"confirm": true, "payment_method": map[string]any{"token": tok.Str("token")}}), http.StatusCreated)
	if pi.Str("status") != "authorized" {
		t.Fatalf("token after restart: %s", pi.Raw)
	}
	status := env2.Must(env2.Do("GET", "/admin/v1/vault/status", testenv.AdminToken, nil), http.StatusOK)
	byKey := status.JSON["cards_by_key"].(map[string]any)
	if status.Str("primary_key") != "k2" || byKey["k1"].(float64) != 1 {
		t.Fatalf("vault status before rewrap: %s", status.Raw)
	}
	env2.Must(env2.Do("POST", "/admin/v1/vault/rewrap", testenv.AdminToken, nil), http.StatusOK)
	status = env2.Must(env2.Do("GET", "/admin/v1/vault/status", testenv.AdminToken, nil), http.StatusOK)
	byKey = status.JSON["cards_by_key"].(map[string]any)
	if byKey["k2"].(float64) != 1 || byKey["k1"] != nil {
		t.Fatalf("vault status after rewrap: %s", status.Raw)
	}
	ver := env2.Must(env2.Do("GET", "/admin/v1/audit_logs/verify", testenv.AdminToken, nil), http.StatusOK)
	if ver.JSON["valid"] != true {
		t.Fatalf("audit chain broken across restart: %s", ver.Raw)
	}
}

// TestConcurrentIdempotentRequests fires the same idempotent request in
// parallel: exactly one payment may be created.
func TestConcurrentIdempotentRequests(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Parallel")
	body := map[string]any{"amount": 2500, "currency": "USD", "confirm": true, "payment_method": testenv.Card("4242424242424242")}
	var wg sync.WaitGroup
	statuses := make(chan int, 10)
	ids := make(chan string, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := env.Do("POST", "/v1/payment_intents", m.Key, body, "Idempotency-Key", "same-order")
			statuses <- r.Status
			if r.Status == http.StatusCreated {
				ids <- r.Str("id")
			}
		}()
	}
	wg.Wait()
	close(statuses)
	close(ids)
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	for s := range statuses {
		if s != http.StatusCreated && s != http.StatusConflict {
			t.Fatalf("unexpected status %d", s)
		}
	}
	if len(unique) != 1 {
		t.Fatalf("created %d distinct payments", len(unique))
	}
	list := env.Must(env.Do("GET", "/v1/payment_intents", m.Key, nil), http.StatusOK)
	if n := len(list.JSON["data"].([]any)); n != 1 {
		t.Fatalf("%d payments stored", n)
	}
}
