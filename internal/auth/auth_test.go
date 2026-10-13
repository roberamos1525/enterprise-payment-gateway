package auth

import (
	"context"
	"testing"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

func TestParseAdminsAndAuthenticate(t *testing.T) {
	a, err := ParseAdmins("ops:aaaaaaaaaaaaaaaaaaaaaaaa, audit:bbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil || a.Count() != 2 {
		t.Fatalf("parse = %v %v", a, err)
	}
	if name, ok := a.Authenticate("bbbbbbbbbbbbbbbbbbbbbbbb"); !ok || name != "audit" {
		t.Fatalf("auth = %q %v", name, ok)
	}
	if _, ok := a.Authenticate("cccccccccccccccccccccccc"); ok {
		t.Fatal("unknown token accepted")
	}
	for _, bad := range []string{"", "ops", "ops:short", "OPS:aaaaaaaaaaaaaaaaaaaaaaaa",
		"ops:aaaaaaaaaaaaaaaaaaaaaaaa,ops:bbbbbbbbbbbbbbbbbbbbbbbb"} {
		if _, err := ParseAdmins(bad); err == nil {
			t.Errorf("ParseAdmins(%q) accepted", bad)
		}
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{"Bearer abc": "abc", "bearer  abc ": "abc", "Basic abc": "", "Bearer": "", "": ""}
	for header, want := range cases {
		got, ok := BearerToken(header)
		if got != want || ok != (want != "") {
			t.Errorf("BearerToken(%q) = %q %v", header, got, ok)
		}
	}
}

func TestMerchantContext(t *testing.T) {
	if _, ok := MerchantFrom(context.Background()); ok {
		t.Fatal("empty context has a merchant")
	}
	ctx := WithMerchant(context.Background(), model.Merchant{ID: "mer_1"}, model.APIKey{ID: "key_1"})
	p, ok := MerchantFrom(ctx)
	if !ok || p.Merchant.ID != "mer_1" || p.Key.ID != "key_1" {
		t.Fatalf("principal = %+v", p)
	}
}
