package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/logging"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
)

var t0 = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

func TestSignAndVerify(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	h := SignatureHeader(t0.Unix(), body, "whsec_a", "whsec_b")
	if !strings.HasPrefix(h, "t=") || strings.Count(h, "v1=") != 2 {
		t.Fatalf("header = %s", h)
	}
	for _, s := range []string{"whsec_a", "whsec_b"} {
		if err := Verify(h, body, s, time.Minute, t0); err != nil {
			t.Fatalf("verify with %s: %v", s, err)
		}
	}
	if err := Verify(h, body, "whsec_c", time.Minute, t0); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("wrong secret: %v", err)
	}
	if err := Verify(h, []byte(`{"id":"evt_2"}`), "whsec_a", time.Minute, t0); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("tampered body: %v", err)
	}
	if err := Verify(h, body, "whsec_a", time.Minute, t0.Add(2*time.Minute)); !errors.Is(err, ErrSignatureTimestamp) {
		t.Fatalf("stale timestamp: %v", err)
	}
	if err := Verify(h, body, "whsec_a", 0, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("zero tolerance disables the timestamp check: %v", err)
	}
	for _, bad := range []string{"", "t=abc,v1=00", "v1=00", "t=123"} {
		if err := Verify(bad, body, "whsec_a", time.Minute, t0); !errors.Is(err, ErrSignatureMissing) {
			t.Errorf("Verify(%q) = %v", bad, err)
		}
	}
}

func TestValidateURL(t *testing.T) {
	strict := URLPolicy{}
	lax := URLPolicy{AllowInsecureHTTP: true, AllowPrivateNetworks: true}
	cases := []struct {
		url    string
		strict bool
		lax    bool
	}{
		{"https://hooks.example.com/p", true, true},
		{"http://hooks.example.com/p", false, true},
		{"https://127.0.0.1:8443/p", false, true},
		{"https://192.168.1.10/p", false, true},
		{"https://100.64.0.1/p", false, true},
		{"https://api.localhost/p", false, true},
		{"https://[fe80::1]/p", false, true},
		{"https://user@hooks.example.com/p", false, false},
		{"https://hooks.example.com/p#frag", false, false},
		{"ftp://hooks.example.com/p", false, false},
		{"/relative", false, false},
		{"https://" + strings.Repeat("a", 2050) + ".com", false, false},
	}
	for _, c := range cases {
		if got := ValidateURL(c.url, strict) == nil; got != c.strict {
			t.Errorf("strict %s: ok=%v", c.url, got)
		}
		if got := ValidateURL(c.url, lax) == nil; got != c.lax {
			t.Errorf("lax %s: ok=%v", c.url, got)
		}
	}
}

func TestPublicAddrAndDialGuard(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "10.0.0.1": false, "127.0.0.1": false, "169.254.1.1": false,
		"::ffff:10.0.0.1": false, "2606:4700::1": true, "198.18.0.1": false, "0.0.0.0": false,
	} {
		if got := PublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("PublicAddr(%s) = %v", addr, got)
		}
	}
	guard := dialGuard(false)
	if err := guard("tcp", "127.0.0.1:80"); err == nil {
		t.Fatal("loopback dial allowed")
	}
	if err := guard("tcp", "8.8.8.8:443"); err != nil {
		t.Fatalf("public dial blocked: %v", err)
	}
	if err := guard("tcp", "example.com:443"); err == nil {
		t.Fatal("unresolved host allowed")
	}
	if err := dialGuard(true)("tcp", "127.0.0.1:80"); err != nil {
		t.Fatal("allowPrivate should permit loopback")
	}
}

func TestBackoff(t *testing.T) {
	base, max := 5*time.Second, time.Minute
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := Backoff(i+1, base, max, false); got != w {
			t.Errorf("Backoff(%d) = %s, want %s", i+1, got, w)
		}
	}
	for i := 0; i < 50; i++ {
		d := Backoff(2, base, max, true)
		if d < 8*time.Second || d > 12*time.Second {
			t.Fatalf("jittered backoff out of range: %s", d)
		}
	}
	if Backoff(0, base, max, false) != base {
		t.Fatal("n<1 should be treated as 1")
	}
}

func TestEventTypes(t *testing.T) {
	if !KnownEvent("*") || !KnownEvent(EventRefundFailed) || KnownEvent("refund.exploded") {
		t.Fatal("KnownEvent wrong")
	}
	types := EventTypes()
	if len(types) != 15 || types[0] > types[1] {
		t.Fatalf("event types = %v", types)
	}
}

// --- outbox + dispatcher against a real store ---------------------------------

type harness struct {
	db  *store.DB
	kr  *vault.Keyring
	clk *clock.Fake
	out *Outbox
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "wh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Q().InsertMerchant(ctx, model.Merchant{ID: "mer_1", Name: "m", Email: "m@x.test", Country: "US",
		DefaultCurrency: "USD", Status: model.MerchantActive, CreatedAt: t0, UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	kr, _ := vault.NewKeyring([]byte("fingerprint-secret-0123"), vault.NamedKey{ID: "k1", Key: bytes.Repeat([]byte{4}, 32)})
	clk := clock.NewFake(t0)
	return &harness{db: db, kr: kr, clk: clk, out: NewOutbox(clk)}
}

func (h *harness) endpoint(t *testing.T, id, url string, events ...string) string {
	t.Helper()
	secret := "whsec_" + id
	sealed, _ := h.kr.SealString(secret, SecretAAD(id))
	if err := h.db.Q().InsertEndpoint(context.Background(), model.WebhookEndpoint{ID: id, MerchantID: "mer_1", URL: url,
		Events: events, Status: model.EndpointEnabled, SecretSealed: sealed, CreatedAt: t0, UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	return secret
}

func (h *harness) publish(t *testing.T, typ string) model.Event {
	t.Helper()
	var ev model.Event
	err := h.db.Tx(context.Background(), func(q *store.Queries) (err error) {
		ev, err = h.out.Publish(context.Background(), q, "mer_1", typ, "pi_1", map[string]string{"id": "pi_1"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func (h *harness) dispatcher(maxAttempts int) *Dispatcher {
	return NewDispatcher(h.db, h.kr, h.clk, DispatchOptions{MaxAttempts: maxAttempts, BaseBackoff: time.Second,
		MaxBackoff: time.Minute, AllowPrivate: true}, logging.Discard(), nil)
}

func TestOutboxFanOutBySubscription(t *testing.T) {
	h := newHarness(t)
	h.endpoint(t, "we_all", "http://127.0.0.1:1", "*")
	h.endpoint(t, "we_cap", "http://127.0.0.1:1", EventPaymentCaptured)
	h.publish(t, EventPaymentCreated)
	h.publish(t, EventPaymentCaptured)
	ds, _, err := h.db.Q().ListDeliveries(context.Background(), store.DeliveryFilter{MerchantID: "mer_1"})
	if err != nil || len(ds) != 3 {
		t.Fatalf("deliveries = %d %v", len(ds), err)
	}
	err = h.db.Tx(context.Background(), func(q *store.Queries) error {
		_, err := h.out.Publish(context.Background(), q, "mer_1", "bogus.event", "x", nil)
		return err
	})
	if err == nil {
		t.Fatal("unknown event type accepted")
	}
}

func TestDispatcherDeliversSignedEnvelope(t *testing.T) {
	h := newHarness(t)
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env Envelope
		_ = json.NewDecoder(r.Body).Decode(&env)
		got.Store([]string{r.Header.Get(HeaderSignature), r.Header.Get(HeaderEventType), r.Header.Get(HeaderAttempt), env.ID})
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	h.endpoint(t, "we_1", srv.URL, "*")
	ev := h.publish(t, EventPaymentAuthorized)
	n, err := h.dispatcher(3).RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("RunOnce = %d %v", n, err)
	}
	vals := got.Load().([]string)
	if vals[1] != EventPaymentAuthorized || vals[2] != "1" || vals[3] != ev.ID || !strings.Contains(vals[0], "v1=") {
		t.Fatalf("request = %v", vals)
	}
	ds, _, _ := h.db.Q().ListDeliveries(context.Background(), store.DeliveryFilter{MerchantID: "mer_1"})
	attempts, _ := h.db.Q().ListAttempts(context.Background(), ds[0].ID)
	if ds[0].Status != model.DeliverySucceeded || len(attempts) != 1 || attempts[0].ResponseSnippet != "ok" {
		t.Fatalf("delivery = %+v attempts = %+v", ds[0], attempts)
	}
}

func TestDispatcherRetriesThenFails(t *testing.T) {
	h := newHarness(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	h.endpoint(t, "we_1", srv.URL, "*")
	h.publish(t, EventPaymentCreated)
	d := h.dispatcher(3)
	for i := 0; i < 3; i++ {
		if n, _ := d.RunOnce(context.Background()); n != 1 {
			t.Fatalf("pass %d attempted %d", i, n)
		}
		if n, _ := d.RunOnce(context.Background()); n != 0 && i < 2 {
			t.Fatal("retry ignored backoff")
		}
		h.clk.Advance(time.Minute)
	}
	ds, _, _ := h.db.Q().ListDeliveries(context.Background(), store.DeliveryFilter{MerchantID: "mer_1"})
	if ds[0].Status != model.DeliveryFailed || ds[0].Attempts != 3 || ds[0].LastStatusCode != 502 || calls.Load() != 3 {
		t.Fatalf("delivery = %+v calls=%d", ds[0], calls.Load())
	}
}

func TestDispatcherConnectionErrorsAndGuard(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more
	h.endpoint(t, "we_down", url, "*")
	h.publish(t, EventPaymentCreated)
	if _, err := h.dispatcher(5).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ds, _, _ := h.db.Q().ListDeliveries(context.Background(), store.DeliveryFilter{MerchantID: "mer_1"})
	if ds[0].Status != model.DeliveryPending || ds[0].LastError == "" || ds[0].Attempts != 1 {
		t.Fatalf("connection failure should be retried: %+v", ds[0])
	}

	// With the guard on, a loopback endpoint is never contacted.
	h2 := newHarness(t)
	var hit atomic.Bool
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	defer live.Close()
	h2.endpoint(t, "we_local", live.URL, "*")
	h2.publish(t, EventPaymentCreated)
	guarded := NewDispatcher(h2.db, h2.kr, h2.clk, DispatchOptions{MaxAttempts: 1}, logging.Discard(), nil)
	if _, err := guarded.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ds2, _, _ := h2.db.Q().ListDeliveries(context.Background(), store.DeliveryFilter{MerchantID: "mer_1"})
	if hit.Load() || ds2[0].Status != model.DeliveryFailed || !strings.Contains(ds2[0].LastError, "non-public") {
		t.Fatalf("guard failed: hit=%v delivery=%+v", hit.Load(), ds2[0])
	}
}

func TestReplayAndPing(t *testing.T) {
	h := newHarness(t)
	h.endpoint(t, "we_1", "http://127.0.0.1:1", EventPaymentCreated)
	h.endpoint(t, "we_2", "http://127.0.0.1:1", EventRefundSucceeded)
	ev := h.publish(t, EventPaymentCreated)
	ctx := context.Background()
	ds, err := h.out.Replay(ctx, h.db, "mer_1", ev.ID, "")
	if err != nil || len(ds) != 1 || ds[0].EndpointID != "we_1" || ds[0].ReplayOf == "" {
		t.Fatalf("replay = %+v %v", ds, err)
	}
	if _, err := h.out.Replay(ctx, h.db, "mer_1", ev.ID, "we_2"); err == nil {
		t.Fatal("replay to non-subscribed endpoint accepted")
	}
	if _, err := h.out.Replay(ctx, h.db, "mer_other", ev.ID, ""); err == nil {
		t.Fatal("replay of another merchant's event accepted")
	}
	d, err := h.out.Ping(ctx, h.db, "mer_1", "we_2")
	if err != nil || d.EndpointID != "we_2" {
		t.Fatalf("ping = %+v %v", d, err)
	}
	if _, err := h.out.Ping(ctx, h.db, "mer_other", "we_2"); err == nil {
		t.Fatal("ping of another merchant's endpoint accepted")
	}
}
