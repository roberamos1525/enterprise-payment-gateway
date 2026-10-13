package idempotency

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

func setup(t *testing.T) (*Service, *clock.Fake) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clk := clock.NewFake(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	return NewService(db, clk, Options{TTL: time.Hour, LockTTL: time.Minute}), clk
}

func TestBeginFinishReplay(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	body := []byte(`{"amount":100}`)
	r, err := s.Begin(ctx, "mer_1", "key-1", "POST", "/v1/payment_intents", body)
	if err != nil || r != nil {
		t.Fatalf("first begin = %v %v", r, err)
	}
	// A concurrent retry while the first is in flight.
	if _, err := s.Begin(ctx, "mer_1", "key-1", "POST", "/v1/payment_intents", body); !apperr.Is(err, apperr.CodeIdempotencyInProgress) {
		t.Fatalf("in-flight retry: %v", err)
	}
	if err := s.Finish(ctx, "mer_1", "key-1", 201, []byte(`{"id":"pi_1"}`)); err != nil {
		t.Fatal(err)
	}
	r, err = s.Begin(ctx, "mer_1", "key-1", "POST", "/v1/payment_intents", body)
	if err != nil || r == nil || r.Status != 201 || string(r.Body) != `{"id":"pi_1"}` {
		t.Fatalf("replay = %+v %v", r, err)
	}
	// Different body, path or method with the same key is misuse.
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/payment_intents", `{"amount":101}`},
		{"POST", "/v1/payment_intents/pi_1/capture", `{"amount":100}`},
		{"PATCH", "/v1/payment_intents", `{"amount":100}`},
	} {
		if _, err := s.Begin(ctx, "mer_1", "key-1", c.method, c.path, []byte(c.body)); !apperr.Is(err, apperr.CodeIdempotencyMismatch) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	// Other merchants have their own key space.
	if r, err := s.Begin(ctx, "mer_2", "key-1", "POST", "/v1/payment_intents", body); err != nil || r != nil {
		t.Fatalf("other merchant = %v %v", r, err)
	}
}

func TestServerErrorsReleaseTheKey(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	if _, err := s.Begin(ctx, "m", "k", "POST", "/p", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, "m", "k", 503, []byte("unavailable")); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Begin(ctx, "m", "k", "POST", "/p", nil); err != nil || r != nil {
		t.Fatalf("retry after 5xx should execute again: %v %v", r, err)
	}
}

func TestStaleLockTakeoverAndExpiry(t *testing.T) {
	s, clk := setup(t)
	ctx := context.Background()
	if _, err := s.Begin(ctx, "m", "k", "POST", "/p", []byte("a")); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute) // original request presumed dead
	if r, err := s.Begin(ctx, "m", "k", "POST", "/p", []byte("a")); err != nil || r != nil {
		t.Fatalf("takeover = %v %v", r, err)
	}
	if err := s.Finish(ctx, "m", "k", 200, []byte("done")); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Hour)
	// Expired: same key with a different body is a fresh request.
	if r, err := s.Begin(ctx, "m", "k", "POST", "/p", []byte("b")); err != nil || r != nil {
		t.Fatalf("after expiry = %v %v", r, err)
	}
	clk.Advance(2 * time.Hour)
	if n, err := s.Purge(ctx); err != nil || n != 1 {
		t.Fatalf("purge = %d %v", n, err)
	}
}

func TestKeyValidationAndFingerprint(t *testing.T) {
	for key, ok := range map[string]bool{"abc-123": true, "": false, "has space": false, strings.Repeat("a", 256): false, "tab\t": false} {
		if ValidKey(key) != ok {
			t.Errorf("ValidKey(%q) != %v", key, ok)
		}
	}
	s, _ := setup(t)
	if _, err := s.Begin(context.Background(), "m", "bad key", "POST", "/p", nil); !apperr.Is(err, apperr.CodeValidation) {
		t.Fatalf("invalid key: %v", err)
	}
	if Fingerprint("POST", "/a", []byte("x")) == Fingerprint("POST", "/ax", nil) {
		t.Fatal("fingerprint fields must be separated")
	}
}
