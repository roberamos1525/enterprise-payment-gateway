package card

import (
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
)

func TestCatalogCardsAreLuhnValidAndBranded(t *testing.T) {
	for _, c := range TestCards() {
		if !LuhnValid(c.PAN) {
			t.Errorf("%s: not Luhn valid", c.PAN)
		}
		if got := DetectBrand(c.PAN); got != c.Brand {
			t.Errorf("%s: brand = %s, want %s", c.PAN, got, c.Brand)
		}
	}
}

func TestLuhn(t *testing.T) {
	cases := map[string]bool{
		"4242424242424242": true,
		"4242424242424241": false,
		"79927398713":      true,
		"0":                false,
		"":                 false,
		"42424242a4242424": false,
	}
	for in, want := range cases {
		if got := LuhnValid(in); got != want {
			t.Errorf("LuhnValid(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLuhnCheckDigit(t *testing.T) {
	if d := LuhnCheckDigit("424242424242424"); d != '2' {
		t.Fatalf("check digit = %c, want 2", d)
	}
	if d := LuhnCheckDigit("7992739871"); d != '3' {
		t.Fatalf("check digit = %c, want 3", d)
	}
}

func TestDetectBrand(t *testing.T) {
	cases := []struct {
		pan  string
		want Brand
	}{
		{"4111111111111111", Visa},
		{"4222222222222", Visa},
		{"5105105105105100", Mastercard},
		{"2720990000000007", Mastercard},
		{"371449635398431", Amex},
		{"30569309025904", Diners},
		{"6011000990139424", Discover},
		{"6445644564456445", Discover},
		{"3530111333300000", JCB},
		{"6759649826438453", Maestro},
		{"9999999999999995", Unknown},
		{"41111111111111", Unknown}, // visa prefix, wrong length
	}
	for _, c := range cases {
		if got := DetectBrand(c.pan); got != c.want {
			t.Errorf("DetectBrand(%s) = %s, want %s", c.pan, got, c.want)
		}
	}
}

func TestMaskAndHelpers(t *testing.T) {
	if got := Mask("4242424242424242"); got != "424242******4242" {
		t.Fatalf("mask = %s", got)
	}
	if got := Mask("378282246310005"); got != "378282*****0005" {
		t.Fatalf("mask = %s", got)
	}
	if got := Mask("12345"); got != "*****" {
		t.Fatalf("short mask = %s", got)
	}
	if BIN("4242424242424242") != "424242" || Last4("4242424242424242") != "4242" {
		t.Fatal("bin/last4 wrong")
	}
	if Normalize("4242 4242-4242 4242") != "4242424242424242" {
		t.Fatal("normalize failed")
	}
}

func TestValidate(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	ok, err := Validate(Input{Number: "4242 4242 4242 4242", ExpMonth: 12, ExpYear: 2030, CVC: "123"}, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok.PAN != "4242424242424242" || ok.Brand != Visa {
		t.Fatalf("unexpected result %+v", ok)
	}

	_, err = Validate(Input{Number: "4242424242424241", ExpMonth: 13, ExpYear: 2020, CVC: "12a"}, now)
	e, isApp := apperr.As(err)
	if !isApp || e.Code != apperr.CodeValidation {
		t.Fatalf("expected validation error, got %v", err)
	}
	fields := map[string]bool{}
	for _, f := range e.Fields {
		fields[f.Field] = true
		if strings.Contains(f.Message, "4242424242424241") {
			t.Fatal("validation message leaked the PAN")
		}
	}
	for _, f := range []string{"card.number", "card.exp_month", "card.cvc"} {
		if !fields[f] {
			t.Errorf("missing field error for %s", f)
		}
	}

	_, err = Validate(Input{Number: "378282246310005", ExpMonth: 1, ExpYear: 2030, CVC: "123"}, now)
	if err == nil {
		t.Fatal("amex with 3-digit cvc should fail")
	}
	_, err = Validate(Input{Number: "4242424242424242", ExpMonth: 5, ExpYear: 2026, CVC: "123"}, now)
	if err == nil {
		t.Fatal("expired card should fail")
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 6, 30, 23, 59, 0, 0, time.UTC)
	if Expired(6, 2026, now) {
		t.Fatal("card valid through end of expiry month")
	}
	if !Expired(6, 2026, now.Add(time.Minute)) {
		t.Fatal("card should expire on the first of the next month")
	}
	if Expired(12, 2026, time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("december rollover handled incorrectly")
	}
}

func TestRedactPANs(t *testing.T) {
	in := "card 4242424242424242 and 4242-4242-4242-4242 but order 12345678901234 and phone 555-1234"
	out := RedactPANs(in)
	if strings.Contains(out, "4242424242424242") || strings.Contains(out, "4242-4242-4242-4242") {
		t.Fatalf("PAN not redacted: %s", out)
	}
	if !strings.Contains(out, "424242******4242") {
		t.Fatalf("expected masked form in %s", out)
	}
	if !strings.Contains(out, "12345678901234") {
		t.Fatalf("non-Luhn number should be untouched: %s", out)
	}
	if RedactPANs("no digits here") != "no digits here" {
		t.Fatal("plain strings must pass through")
	}
}

func TestGeneratePAN(t *testing.T) {
	for i := 0; i < 50; i++ {
		pan, err := GeneratePAN("400000", 16, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(pan) != 16 || !strings.HasPrefix(pan, "400000") || !LuhnValid(pan) {
			t.Fatalf("bad generated pan %s", pan)
		}
	}
	if _, err := GeneratePAN("4x", 16, nil); err == nil {
		t.Fatal("expected error for bad prefix")
	}
	if _, err := GeneratePAN("4000000000000000", 16, nil); err == nil {
		t.Fatal("expected error when prefix fills the length")
	}
}

func TestIssuerCountryAndFunding(t *testing.T) {
	if IssuerCountry("4000008260000000") != "GB" {
		t.Fatal("expected GB issuer")
	}
	if IssuerCountry("3530111333300000") != "JP" {
		t.Fatal("expected JP default for JCB")
	}
	if Funding("4000056655665556") != "debit" || Funding("4111111111111111") != "credit" {
		t.Fatal("funding lookup wrong")
	}
	if _, ok := LookupTestCard("4111111111111111"); ok {
		t.Fatal("non-catalog card found")
	}
}
