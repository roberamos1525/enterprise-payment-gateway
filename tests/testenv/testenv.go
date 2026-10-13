// Package testenv builds a fully wired, in-process gateway for tests: a
// temporary SQLite database, a fake clock, zero acquirer latency and an
// httptest server in front of the real HTTP handler.
package testenv

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/app"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/internal/merchant"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
)

// AdminToken is the operator token configured for every test environment.
const AdminToken = "test-admin-token-0123456789abcdef"

// Start is the fake clock's initial time (a Monday, mid-morning UTC).
var Start = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

// Env is a running test gateway.
type Env struct {
	T      testing.TB
	App    *app.App
	Clock  *clock.Fake
	Server *httptest.Server
	Logs   *SafeBuffer
	DBPath string
}

// SafeBuffer is a goroutine-safe log sink.
type SafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *SafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything logged so far.
func (b *SafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Config returns the base test configuration.
func Config(dir string) config.Config {
	cfg := config.Default()
	cfg.Env = "dev"
	cfg.DBPath = filepath.Join(dir, "gateway.db")
	cfg.VaultKeys = "k1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	cfg.FingerprintSecret = "test-fingerprint-secret-0123456789"
	cfg.AdminTokens = "ops:" + AdminToken
	cfg.Acquirer.BaseLatency, cfg.Acquirer.Jitter, cfg.Acquirer.SlowLatency = config.D(0), config.D(0), config.D(0)
	cfg.RateLimit.MerchantRPS, cfg.RateLimit.MerchantBurst = 10_000, 10_000
	cfg.RateLimit.AdminRPS, cfg.RateLimit.AdminBurst = 10_000, 10_000
	cfg.RateLimit.AnonRPS, cfg.RateLimit.AnonBurst = 10_000, 10_000
	cfg.Webhooks.AllowInsecureHTTP, cfg.Webhooks.AllowPrivateNetworks = true, true
	cfg.Webhooks.RequestTimeout = config.D(5 * time.Second)
	cfg.Settle.PayoutDelayDays = 1
	return cfg
}

// New starts an environment. mutate may adjust the configuration.
func New(t testing.TB, mutate ...func(*config.Config)) *Env {
	t.Helper()
	dir := t.TempDir()
	cfg := Config(dir)
	for _, m := range mutate {
		m(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	logs := &SafeBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	clk := clock.NewFake(Start)
	a, err := app.New(context.Background(), cfg, app.Options{Clock: clk, Log: log, Version: "test"})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(func() {
		srv.Close()
		_ = a.Stop()
	})
	return &Env{T: t, App: a, Clock: clk, Server: srv, Logs: logs, DBPath: cfg.DBPath}
}

// Merchant is an onboarded, active merchant with an API key.
type Merchant struct {
	model.Merchant
	Key string
}

// Onboard creates an active merchant and an API key through the services.
func (e *Env) Onboard(name string) Merchant {
	e.T.Helper()
	ctx := context.Background()
	m, err := e.App.Merchants.Onboard(ctx, merchant.OnboardInput{
		Name: name, Email: strings.ToLower(strings.ReplaceAll(name, " ", "")) + "@example.test",
		Country: "US", DefaultCurrency: "USD", Activate: true,
	})
	if err != nil {
		e.T.Fatalf("onboard: %v", err)
	}
	k, err := e.App.Merchants.CreateKey(ctx, m.ID, merchant.CreateKeyInput{Label: "test"})
	if err != nil {
		e.T.Fatalf("create key: %v", err)
	}
	return Merchant{Merchant: m, Key: k.Secret}
}

// Response is a decoded HTTP response.
type Response struct {
	Status int
	Header http.Header
	Raw    []byte
	JSON   map[string]any
}

// Str returns a top-level string field.
func (r Response) Str(key string) string {
	s, _ := r.JSON[key].(string)
	return s
}

// Num returns a top-level numeric field.
func (r Response) Num(key string) int64 {
	f, _ := r.JSON[key].(float64)
	return int64(f)
}

// ErrType returns error.type of an error envelope.
func (r Response) ErrType() string {
	e, _ := r.JSON["error"].(map[string]any)
	s, _ := e["type"].(string)
	return s
}

// Do sends a request with a bearer token (may be empty) and optional JSON body.
func (e *Env) Do(method, path, token string, body any, headers ...string) Response {
	e.T.Helper()
	var rdr io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rdr = strings.NewReader(b)
		case []byte:
			rdr = bytes.NewReader(b)
		default:
			raw, err := json.Marshal(b)
			if err != nil {
				e.T.Fatalf("marshal: %v", err)
			}
			rdr = bytes.NewReader(raw)
		}
	}
	req, err := http.NewRequest(method, e.Server.URL+path, rdr)
	if err != nil {
		e.T.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := e.Server.Client().Do(req)
	if err != nil {
		e.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := Response{Status: resp.StatusCode, Header: resp.Header, Raw: raw}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(raw, &out.JSON)
	}
	return out
}

// Must asserts the response status.
func (e *Env) Must(r Response, status int) Response {
	e.T.Helper()
	if r.Status != status {
		e.T.Fatalf("status = %d, want %d; body: %s", r.Status, status, r.Raw)
	}
	return r
}

// Card returns a payment_method body for a test PAN.
func Card(pan string) map[string]any {
	return map[string]any{"card": map[string]any{"number": pan, "exp_month": 12, "exp_year": 2030, "cvc": cvcFor(pan)}}
}

func cvcFor(pan string) string {
	if strings.HasPrefix(pan, "34") || strings.HasPrefix(pan, "37") {
		return "1234"
	}
	return "123"
}

// Keyring returns a keyring identical to the test configuration's.
func Keyring(t testing.TB) *vault.Keyring {
	cfg := Config(t.TempDir())
	kr, err := vault.ParseKeyring(cfg.VaultKeys, cfg.FingerprintSecret)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

// DispatchAll runs the webhook dispatcher until no delivery is due.
func (e *Env) DispatchAll() int {
	e.T.Helper()
	total := 0
	for i := 0; i < 20; i++ {
		n, err := e.App.Dispatcher.RunOnce(context.Background())
		if err != nil {
			e.T.Fatalf("dispatch: %v", err)
		}
		if n == 0 {
			return total
		}
		total += n
	}
	return total
}
