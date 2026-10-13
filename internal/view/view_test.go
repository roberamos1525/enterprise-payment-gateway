package view

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

func TestPaymentViewDerivedFields(t *testing.T) {
	now := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	p := &model.PaymentIntent{
		ID: "pi_1", MerchantID: "mer_1", Amount: 12345, Currency: "USD", Status: model.StatusAuthorized,
		CaptureMethod: model.CaptureManual, AmountAuthorized: 12345, CreatedAt: now, UpdatedAt: now,
		Card: &model.CardSummary{Token: "tok_1", Brand: "visa", BIN: "424242", Last4: "4242"},
		Risk: model.Risk{Score: 10, Action: "allow"},
	}
	v := PaymentIntent(p)
	if v.AmountDisplay != "123.45 USD" || v.AmountCapturable != 12345 || v.AmountRefundable != 0 || v.Object != "payment_intent" {
		t.Fatalf("view = %+v", v)
	}
	if v.Card.Masked != "424242******4242" || v.Risk.Reasons == nil || v.Metadata == nil || v.NextAction != nil {
		t.Fatalf("card/risk/metadata = %+v", v)
	}
	p.Status, p.ChallengeID = model.StatusRequiresAction, "3ds_1"
	if v := PaymentIntent(p); v.NextAction == nil || v.NextAction.ChallengeID != "3ds_1" || v.AmountCapturable != 0 {
		t.Fatalf("requires_action view = %+v", v.NextAction)
	}
	p.Status = model.StatusRequiresReview
	if v := PaymentIntent(p); v.NextAction == nil || v.NextAction.Type != "manual_review" {
		t.Fatal("review next_action missing")
	}
	raw, _ := json.Marshal(PaymentIntent(p))
	if !strings.Contains(string(raw), `"masked_number":"424242******4242"`) || strings.Contains(string(raw), "4242424242424242") {
		t.Fatalf("card rendering: %s", raw)
	}
}

func TestListEnvelope(t *testing.T) {
	l := NewList([]string{"a", "b"}, true, func(s string) string { return s })
	if l.NextCursor != "b" || !l.HasMore || l.Object != "list" {
		t.Fatalf("list = %+v", l)
	}
	empty := NewList[string](nil, false, func(s string) string { return s })
	raw, _ := json.Marshal(empty)
	if !strings.Contains(string(raw), `"data":[]`) || strings.Contains(string(raw), "next_cursor") {
		t.Fatalf("empty list json = %s", raw)
	}
	r := RefundOf(model.Refund{ID: "re_1", Amount: 5, Currency: "JPY", Status: model.RefundSucceeded})
	if r.AmountDisplay != "5 JPY" || r.Object != "refund" {
		t.Fatalf("refund view = %+v", r)
	}
	vc := VaultCard(model.VaultCard{Token: "tok", BIN: "378282", Last4: "0005", Brand: "amex"})
	if vc.Masked != "378282******0005" {
		t.Fatalf("vault card = %+v", vc)
	}
}
