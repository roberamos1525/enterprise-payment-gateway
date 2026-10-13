package merchant_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/internal/merchant"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

func TestOnboardValidationAndDefaults(t *testing.T) {
	env := testenv.New(t)
	svc := env.App.Merchants
	ctx := context.Background()
	_, err := svc.Onboard(ctx, merchant.OnboardInput{Name: " ", Email: "nope", Country: "usa", DefaultCurrency: "XYZ",
		Fees: &merchant.FeeInput{PercentBps: 20_000, FixedMinor: -1}})
	e, ok := apperr.As(err)
	if !ok || e.Code != apperr.CodeValidation || len(e.Fields) != 6 {
		t.Fatalf("validation = %+v", err)
	}
	m, err := svc.Onboard(ctx, merchant.OnboardInput{Name: "  Shop  ", Email: "Owner@Shop.TEST", Country: "gb", DefaultCurrency: "gbp"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != model.MerchantPending || m.Name != "Shop" || m.Email != "owner@shop.test" || m.Country != "GB" || m.DefaultCurrency != "GBP" {
		t.Fatalf("normalisation: %+v", m)
	}
	if _, err := svc.Onboard(ctx, merchant.OnboardInput{Name: "Dup", Email: "owner@shop.test", Country: "GB", DefaultCurrency: "GBP"}); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("duplicate email: %v", err)
	}
	fees, err := svc.FeeSchedules(ctx, m.ID)
	if err != nil || len(fees) != 1 || fees[0].PercentBps != 290 || fees[0].FixedMinor != 30 {
		t.Fatalf("default fees = %+v %v", fees, err)
	}
	name := "New Name"
	up, err := svc.UpdateProfile(ctx, m.ID, merchant.UpdateProfileInput{Name: &name})
	if err != nil || up.Name != "New Name" || up.Email != "owner@shop.test" {
		t.Fatalf("update = %+v %v", up, err)
	}
	list, _, err := svc.List(ctx, store.MerchantFilter{Status: model.MerchantPending})
	if err != nil || len(list) != 1 {
		t.Fatalf("list pending = %d %v", len(list), err)
	}
}

func TestStatusTransitions(t *testing.T) {
	cases := []struct {
		from, to model.MerchantStatus
		ok       bool
	}{
		{model.MerchantPending, model.MerchantActive, true},
		{model.MerchantPending, model.MerchantSuspended, false},
		{model.MerchantActive, model.MerchantSuspended, true},
		{model.MerchantSuspended, model.MerchantActive, true},
		{model.MerchantActive, model.MerchantClosed, true},
		{model.MerchantClosed, model.MerchantActive, false},
		{model.MerchantActive, model.MerchantActive, false},
	}
	for _, c := range cases {
		if merchant.CanTransition(c.from, c.to) != c.ok {
			t.Errorf("%s -> %s: want %v", c.from, c.to, c.ok)
		}
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	env := testenv.New(t)
	svc := env.App.Merchants
	ctx := context.Background()
	m := env.Onboard("Keys")
	if prefix, ok := merchant.ParseKey(m.Key); !ok || !strings.HasPrefix(m.Key, prefix+"_") {
		t.Fatalf("ParseKey(%q) = %q %v", m.Key, prefix, ok)
	}
	for _, bad := range []string{"", "pk_test_x", "sk_test_short_secret", "sk_test_abcdefgh_" + strings.Repeat("x", 31)} {
		if _, ok := merchant.ParseKey(bad); ok {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
	if _, _, err := svc.Authenticate(ctx, m.Key); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if keys, _ := svc.ListKeys(ctx, m.ID); keys[0].LastUsedAt == nil {
		t.Fatal("last_used_at not recorded")
	}
	past := env.Clock.Now().Add(-time.Hour)
	if _, err := svc.CreateKey(ctx, m.ID, merchant.CreateKeyInput{ExpiresAt: &past}); !apperr.Is(err, apperr.CodeValidation) {
		t.Fatalf("past expiry: %v", err)
	}
	future := env.Clock.Now().Add(time.Hour)
	short, err := svc.CreateKey(ctx, m.ID, merchant.CreateKeyInput{Label: "temp", ExpiresAt: &future})
	if err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(2 * time.Hour)
	if _, _, err := svc.Authenticate(ctx, short.Secret); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("expired key: %v", err)
	}
	if _, err := svc.RotateKey(ctx, m.ID, short.ID, nil); !apperr.Is(err, apperr.CodeInvalidState) {
		t.Fatalf("rotate expired: %v", err)
	}
	keys, _ := svc.ListKeys(ctx, m.ID)
	orig := keys[len(keys)-1]
	zero := time.Duration(0)
	rotated, err := svc.RotateKey(ctx, m.ID, orig.ID, &zero)
	if err != nil || rotated.RotatedFrom != orig.ID {
		t.Fatalf("rotate = %+v %v", rotated, err)
	}
	if _, _, err := svc.Authenticate(ctx, m.Key); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatal("zero-grace rotation should revoke immediately")
	}
	tooLong := 40 * 24 * time.Hour
	if _, err := svc.RotateKey(ctx, m.ID, rotated.ID, &tooLong); !apperr.Is(err, apperr.CodeValidation) {
		t.Fatalf("grace too long: %v", err)
	}
	if err := svc.RevokeKey(ctx, m.ID, rotated.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeKey(ctx, m.ID, rotated.ID); !apperr.Is(err, apperr.CodeInvalidState) {
		t.Fatalf("double revoke: %v", err)
	}
	other := env.Onboard("Other")
	if err := svc.RevokeKey(ctx, other.ID, short.ID); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("revoke another merchant's key: %v", err)
	}
}

func TestEndpointManagement(t *testing.T) {
	env := testenv.New(t, func(c *config.Config) { c.Webhooks.MaxEndpoints = 2 })
	svc := env.App.Merchants
	ctx := context.Background()
	m := env.Onboard("Endpoints")
	url, events := "https://hooks.example.com/a", []string{"payment_intent.captured", "payment_intent.captured", "refund.succeeded"}
	ep, err := svc.CreateEndpoint(ctx, m.ID, merchant.EndpointInput{URL: &url, Events: &events})
	if err != nil || len(ep.Events) != 2 || !strings.HasPrefix(ep.Secret, "whsec_") || strings.Contains(ep.SecretSealed, ep.Secret) {
		t.Fatalf("create = %+v %v", ep, err)
	}
	if _, err := svc.CreateEndpoint(ctx, m.ID, merchant.EndpointInput{URL: &url}); !apperr.Is(err, apperr.CodeValidation) {
		t.Fatalf("missing events: %v", err)
	}
	disabled := false
	if _, err := svc.CreateEndpoint(ctx, m.ID, merchant.EndpointInput{URL: &url, Events: &events, Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateEndpoint(ctx, m.ID, merchant.EndpointInput{URL: &url, Events: &events}); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("endpoint limit: %v", err)
	}
	desc := "updated"
	up, err := svc.UpdateEndpoint(ctx, m.ID, ep.ID, merchant.EndpointInput{Description: &desc})
	if err != nil || up.Description != "updated" || up.URL != url {
		t.Fatalf("update = %+v %v", up, err)
	}
	other := env.Onboard("Other")
	if _, err := svc.GetEndpoint(ctx, other.ID, ep.ID); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("cross-merchant endpoint read: %v", err)
	}
	rot, err := svc.RotateSecret(ctx, m.ID, ep.ID)
	if err != nil || rot.Secret == ep.Secret || rot.PrevSecretExpiresAt == nil {
		t.Fatalf("rotate = %+v %v", rot, err)
	}
	opened, err := env.App.Keyring.OpenString(rot.PrevSecretSealed, "whsec|"+ep.ID)
	if err != nil || opened != ep.Secret {
		t.Fatalf("previous secret not retained: %v", err)
	}
}
