package vault

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func spec(id string, b byte) string { return id + ":" + base64.StdEncoding.EncodeToString(key(b)) }

func TestParseKeyring(t *testing.T) {
	kr, err := ParseKeyring(spec("k2", 2)+","+spec("k1", 1), "fingerprint-secret-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if kr.PrimaryID() != "k2" || len(kr.KeyIDs()) != 2 {
		t.Fatalf("primary=%s ids=%v", kr.PrimaryID(), kr.KeyIDs())
	}
	bad := []struct{ spec, fp string }{
		{"", "fingerprint-secret-xyz"},
		{"k1:notbase64!!", "fingerprint-secret-xyz"},
		{"k1:" + base64.StdEncoding.EncodeToString([]byte("short")), "fingerprint-secret-xyz"},
		{spec("k1", 1) + "," + spec("k1", 2), "fingerprint-secret-xyz"},
		{spec("bad id", 1), "fingerprint-secret-xyz"},
		{spec("k1", 1), "short"},
	}
	for _, b := range bad {
		if _, err := ParseKeyring(b.spec, b.fp); err == nil {
			t.Errorf("ParseKeyring(%q) should fail", b.spec)
		}
	}
}

func TestSealOpenAndTamper(t *testing.T) {
	kr, _ := NewKeyring([]byte("fp-secret-0123456789"), NamedKey{"k1", key(1)})
	kid, sealed, err := kr.Seal([]byte("secret"), []byte("aad"))
	if err != nil || kid != "k1" {
		t.Fatal(err)
	}
	pt, err := kr.Open(kid, sealed, []byte("aad"))
	if err != nil || string(pt) != "secret" {
		t.Fatalf("open = %q %v", pt, err)
	}
	if _, err := kr.Open(kid, sealed, []byte("other")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong aad: %v", err)
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err := kr.Open(kid, tampered, []byte("aad")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := kr.Open("nope", sealed, nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := kr.Open(kid, []byte{1, 2}, nil); !errors.Is(err, ErrMalformed) {
		t.Fatalf("short ciphertext: %v", err)
	}
	// Nonces are random: sealing twice differs.
	_, s2, _ := kr.Seal([]byte("secret"), []byte("aad"))
	if bytes.Equal(sealed, s2) {
		t.Fatal("ciphertexts should differ")
	}
}

func TestSealString(t *testing.T) {
	kr, _ := NewKeyring([]byte("fp-secret-0123456789"), NamedKey{"k1", key(1)})
	s, err := kr.SealString("whsec_abc", "endpoint-1")
	if err != nil || !strings.HasPrefix(s, "v1.k1.") || strings.Contains(s, "whsec_abc") {
		t.Fatalf("sealed = %q %v", s, err)
	}
	if SealedKeyID(s) != "k1" || SealedKeyID("garbage") != "" {
		t.Fatal("SealedKeyID wrong")
	}
	pt, err := kr.OpenString(s, "endpoint-1")
	if err != nil || pt != "whsec_abc" {
		t.Fatalf("open = %q %v", pt, err)
	}
	if _, err := kr.OpenString(s, "endpoint-2"); err == nil {
		t.Fatal("wrong aad accepted")
	}
	for _, bad := range []string{"v2.k1.xx", "v1.k1.!!!", "nodots"} {
		if _, err := kr.OpenString(bad, ""); err == nil {
			t.Errorf("OpenString(%q) should fail", bad)
		}
	}
}

func TestFingerprintStableAcrossKeys(t *testing.T) {
	a, _ := NewKeyring([]byte("same-fingerprint-secret"), NamedKey{"k1", key(1)})
	b, _ := NewKeyring([]byte("same-fingerprint-secret"), NamedKey{"k9", key(9)})
	c, _ := NewKeyring([]byte("other-fingerprint-secret"), NamedKey{"k1", key(1)})
	pan := "4242424242424242"
	if a.Fingerprint(pan) != b.Fingerprint(pan) {
		t.Fatal("fingerprint must not depend on encryption keys")
	}
	if a.Fingerprint(pan) == c.Fingerprint(pan) || a.Fingerprint(pan) == a.Fingerprint("4000056655665556") {
		t.Fatal("fingerprint collision")
	}
	if strings.Contains(a.Fingerprint(pan), "4242") {
		t.Fatal("fingerprint reveals digits")
	}
}

func TestTokenizeRevealAndRewrap(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	if err := db.Q().InsertMerchant(ctx, model.Merchant{ID: "mer_v", Name: "v", Email: "v@x.test", Country: "US",
		DefaultCurrency: "USD", Status: model.MerchantActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	old, _ := NewKeyring([]byte("fingerprint-secret-1"), NamedKey{"k1", key(1)})
	svc := NewService(old, clock.NewFake(now))
	c, err := card.Validate(card.Input{Number: "4000008260000000", ExpMonth: 1, ExpYear: 2031, CVC: "999"}, now)
	if err != nil {
		t.Fatal(err)
	}
	vc, err := svc.Tokenize(ctx, db.Q(), "mer_v", c)
	if err != nil {
		t.Fatal(err)
	}
	if vc.Country != "GB" || vc.Last4 != "0000" || vc.BIN != "400000" || bytes.Contains(vc.Ciphertext, []byte("4000008260000000")) {
		t.Fatalf("vault card = %+v", vc)
	}
	if _, err := svc.Lookup(ctx, db.Q(), "mer_other", vc.Token); err == nil {
		t.Fatal("other merchant could look up token")
	}
	got, err := svc.Lookup(ctx, db.Q(), "mer_v", vc.Token)
	if err != nil {
		t.Fatal(err)
	}
	pan, err := svc.Reveal(got)
	if err != nil || pan != "4000008260000000" {
		t.Fatalf("reveal = %q %v", pan, err)
	}
	// Ciphertext is bound to its token: swapping rows fails.
	swapped := got
	swapped.Token = "tok_other"
	if _, err := svc.Reveal(swapped); err == nil {
		t.Fatal("ciphertext opened under a different token")
	}

	// Rotate: new primary k2, old k1 still readable; rewrap moves cards to k2.
	rotated, _ := NewKeyring([]byte("fingerprint-secret-1"), NamedKey{"k2", key(2)}, NamedKey{"k1", key(1)})
	svc2 := NewService(rotated, clock.NewFake(now))
	res, err := svc2.Rewrap(ctx, db, 10)
	if err != nil || res.Rewrapped != 1 || res.Failed != 0 || res.PrimaryKey != "k2" {
		t.Fatalf("rewrap = %+v %v", res, err)
	}
	counts, _ := db.Q().CountCardsByKey(ctx)
	if counts["k2"] != 1 || counts["k1"] != 0 {
		t.Fatalf("counts = %v", counts)
	}
	after, _ := svc2.Lookup(ctx, db.Q(), "mer_v", vc.Token)
	if pan, err := svc2.Reveal(after); err != nil || pan != "4000008260000000" {
		t.Fatalf("reveal after rewrap = %q %v", pan, err)
	}
	// A keyring without k1 can still read rewrapped cards.
	only2, _ := NewKeyring([]byte("fingerprint-secret-1"), NamedKey{"k2", key(2)})
	if _, err := NewService(only2, clock.Real{}).Reveal(after); err != nil {
		t.Fatalf("k2-only reveal: %v", err)
	}
	if res, _ := svc2.Rewrap(ctx, db, 10); res.Rewrapped != 0 {
		t.Fatalf("second rewrap should be a no-op: %+v", res)
	}
}
