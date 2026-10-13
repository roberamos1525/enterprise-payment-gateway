package payment_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/internal/ledger"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/payment"
	"github.com/robertramos/enterprise-payment-gateway/internal/reqctx"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

func cardIn(pan string) *card.Input {
	return &card.Input{Number: pan, ExpMonth: 12, ExpYear: 2030, CVC: "123"}
}

func authorized(t *testing.T, env *testenv.Env, m model.Merchant, amount int64) *model.PaymentIntent {
	t.Helper()
	p, err := env.App.Payments.Create(context.Background(), m, payment.CreateInput{Amount: amount, Currency: "USD",
		Confirm: true, PaymentMethod: &payment.AuthorizeInput{Card: cardIn("4242424242424242")}})
	if err != nil || p.Status != model.StatusAuthorized {
		t.Fatalf("authorize: %v %+v", err, p)
	}
	return p
}

func TestCreateValidation(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Validator").Merchant
	ctx := context.Background()
	bad := []payment.CreateInput{
		{Amount: 0, Currency: "USD"},
		{Amount: 200_000_000, Currency: "USD"},
		{Amount: 100, Currency: "ABC"},
		{Amount: 100, Currency: "USD", CaptureMethod: "eventually"},
		{Amount: 100, Currency: "USD", Confirm: true},
		{Amount: 100, Currency: "USD", PaymentMethod: &payment.AuthorizeInput{Token: "tok_x"}},
		{Amount: 100, Currency: "USD", BillingCountry: "usa"},
		{Amount: 100, Currency: "USD", Metadata: map[string]string{"": "empty key"}},
	}
	for i, in := range bad {
		if _, err := env.App.Payments.Create(ctx, m, in); !apperr.Is(err, apperr.CodeValidation) {
			t.Errorf("case %d: err = %v", i, err)
		}
	}
	p, err := env.App.Payments.Create(ctx, m, payment.CreateInput{Amount: 500, Currency: "jpy"})
	if err != nil || p.Currency != "JPY" || p.CaptureMethod != model.CaptureManual || p.Metadata == nil {
		t.Fatalf("create = %+v %v", p, err)
	}
	m.Status = model.MerchantPending
	if _, err := env.App.Payments.Create(ctx, m, payment.CreateInput{Amount: 1, Currency: "USD"}); !apperr.Is(err, apperr.CodeForbidden) {
		t.Fatalf("pending merchant: %v", err)
	}
}

func TestAuthorizeRequiresExactlyOnePaymentMethod(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Methods").Merchant
	ctx := context.Background()
	p, _ := env.App.Payments.Create(ctx, m, payment.CreateInput{Amount: 100, Currency: "USD"})
	for _, in := range []payment.AuthorizeInput{{}, {Card: cardIn("4242424242424242"), Token: "tok_1"}} {
		if _, err := env.App.Payments.Authorize(ctx, m, p.ID, in); !apperr.Is(err, apperr.CodeValidation) {
			t.Fatalf("input %+v: %v", in, err)
		}
	}
	if _, err := env.App.Payments.Authorize(ctx, m, p.ID, payment.AuthorizeInput{Card: cardIn("4242424242424241")}); !apperr.Is(err, apperr.CodeValidation) {
		t.Fatalf("luhn failure: %v", err)
	}
	// An expired token is declined without reaching the acquirer.
	vc, err := env.App.Payments.Tokenize(ctx, m, card.Input{Number: "4242424242424242", ExpMonth: 3, ExpYear: 2026, CVC: "123"})
	if err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(40 * 24 * time.Hour)
	_, err = env.App.Payments.Authorize(ctx, m, p.ID, payment.AuthorizeInput{Token: vc.Token})
	if e, ok := apperr.As(err); !ok || e.DeclineCode != "expired_card" {
		t.Fatalf("expired token: %v", err)
	}
}

func TestCaptureRulesAndLedger(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Capturer").Merchant
	ctx := context.Background()
	p := authorized(t, env, m, 10_000)
	over := int64(10_001)
	if _, err := env.App.Payments.Capture(ctx, m, p.ID, payment.CaptureInput{Amount: &over}); !apperr.Is(err, apperr.CodeValidation) {
		t.Fatalf("over-capture: %v", err)
	}
	part := int64(6_000)
	got, err := env.App.Payments.Capture(ctx, m, p.ID, payment.CaptureInput{Amount: &part})
	if err != nil || got.AmountCaptured != 6_000 || got.AuthExpiresAt != nil || got.CaptureRef == "" {
		t.Fatalf("capture = %+v %v", got, err)
	}
	if _, err := env.App.Payments.Capture(ctx, m, p.ID, payment.CaptureInput{}); !apperr.Is(err, apperr.CodeInvalidState) {
		t.Fatalf("second capture: %v", err)
	}
	hist, _ := env.App.Payments.History(ctx, m.ID, p.ID)
	if hist[len(hist)-1].Detail != "partial capture; released 40.00" {
		t.Fatalf("history detail = %q", hist[len(hist)-1].Detail)
	}
	bals, _ := ledger.Balances(ctx, env.App.DB.Q(), m.ID)
	for _, b := range bals {
		if b.Account == ledger.MerchantPayable && b.Balance != 6_000 {
			t.Fatalf("merchant payable = %d", b.Balance)
		}
	}
}

func TestMultiplePartialRefunds(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Refunder").Merchant
	ctx := context.Background()
	p := authorized(t, env, m, 9_000)
	if _, err := env.App.Payments.Capture(ctx, m, p.ID, payment.CaptureInput{}); err != nil {
		t.Fatal(err)
	}
	for i, amt := range []int64{1_000, 2_000, 3_000} {
		a := amt
		r, got, err := env.App.Payments.Refund(ctx, m, p.ID, payment.RefundInput{Amount: &a})
		if err != nil || r.Status != model.RefundSucceeded || got.Status != model.StatusPartiallyRefunded {
			t.Fatalf("refund %d: %+v %+v %v", i, r, got, err)
		}
	}
	tooMuch := int64(3_001)
	if _, _, err := env.App.Payments.Refund(ctx, m, p.ID, payment.RefundInput{Amount: &tooMuch}); !apperr.Is(err, apperr.CodeValidation) {
		t.Fatalf("over-refund: %v", err)
	}
	if _, _, err := env.App.Payments.Refund(ctx, m, p.ID, payment.RefundInput{Reason: "because"}); !apperr.Is(err, apperr.CodeValidation) {
		t.Fatalf("bad reason: %v", err)
	}
	r, got, err := env.App.Payments.Refund(ctx, m, p.ID, payment.RefundInput{Reason: "duplicate"})
	if err != nil || r.Amount != 3_000 || got.Status != model.StatusRefunded || got.Refundable() != 0 {
		t.Fatalf("final refund: %+v %+v %v", r, got, err)
	}
	refunds, _ := env.App.Payments.Refunds(ctx, m.ID, p.ID)
	if len(refunds) != 4 {
		t.Fatalf("refunds = %d", len(refunds))
	}
	if _, err := env.App.Payments.GetRefund(ctx, "mer_other", r.ID); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("cross-merchant refund read: %v", err)
	}
}

func TestConcurrentCapturesOnlyOneWins(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Racer").Merchant
	p := authorized(t, env, m, 5_000)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.App.Payments.Capture(context.Background(), m, p.ID, payment.CaptureInput{})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !apperr.Is(err, apperr.CodeInvalidState) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d captures succeeded", wins)
	}
	got, _ := env.App.Payments.Get(context.Background(), m.ID, p.ID)
	if got.AmountCaptured != 5_000 {
		t.Fatalf("captured = %d", got.AmountCaptured)
	}
}

func TestAcquirerTimeoutIsRetryable(t *testing.T) {
	env := testenv.New(t, func(c *config.Config) {
		c.Acquirer.SlowLatency = config.D(2 * time.Second)
		c.Payments.AcquirerTimeout = config.D(50 * time.Millisecond)
	})
	m := env.Onboard("Slowpoke").Merchant
	ctx := context.Background()
	p, _ := env.App.Payments.Create(ctx, m, payment.CreateInput{Amount: 100, Currency: "USD"})
	_, err := env.App.Payments.Authorize(ctx, m, p.ID, payment.AuthorizeInput{Card: cardIn("4000000000000341")})
	if !apperr.Is(err, apperr.CodeUnavailable) {
		t.Fatalf("timeout should be 503: %v", err)
	}
	got, _ := env.App.Payments.Get(ctx, m.ID, p.ID)
	if got.Status != model.StatusCreated {
		t.Fatalf("status after timeout = %s", got.Status)
	}
	// The same intent can be authorized afterwards with a fast card.
	if _, err := env.App.Payments.Authorize(ctx, m, p.ID, payment.AuthorizeInput{Card: cardIn("4242424242424242")}); err != nil {
		t.Fatalf("retry after timeout: %v", err)
	}
}

func TestReviewRequiresPendingReview(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Reviewer").Merchant
	ctx := reqctx.WithActor(context.Background(), reqctx.Actor{Type: reqctx.ActorAdmin, ID: "ops"})
	p := authorized(t, env, m, 100)
	if _, err := env.App.Payments.Review(ctx, p.ID, payment.ReviewInput{Decision: "approve"}); !apperr.Is(err, apperr.CodeInvalidState) {
		t.Fatalf("review of authorized: %v", err)
	}
	if _, err := env.App.Payments.Review(ctx, p.ID, payment.ReviewInput{Decision: "maybe"}); !apperr.Is(err, apperr.CodeValidation) {
		t.Fatalf("bad decision: %v", err)
	}
	if _, err := env.App.Payments.Review(ctx, "pi_missing", payment.ReviewInput{Decision: "reject"}); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("missing payment: %v", err)
	}
}

func TestExpiryJobSkipsCapturedPayments(t *testing.T) {
	env := testenv.New(t)
	m := env.Onboard("Expirer").Merchant
	ctx := context.Background()
	a := authorized(t, env, m, 100)
	b := authorized(t, env, m, 200)
	if _, err := env.App.Payments.Capture(ctx, m, b.ID, payment.CaptureInput{}); err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(6 * 24 * time.Hour)
	if n, _ := env.App.Payments.ExpireAuthorizations(ctx, 10); n != 0 {
		t.Fatalf("expired too early: %d", n)
	}
	env.Clock.Advance(2 * 24 * time.Hour)
	if n, err := env.App.Payments.ExpireAuthorizations(ctx, 10); n != 1 || err != nil {
		t.Fatalf("expired = %d %v", n, err)
	}
	got, _ := env.App.Payments.Get(ctx, m.ID, a.ID)
	if got.Status != model.StatusVoided {
		t.Fatalf("status = %s", got.Status)
	}
	entries, _ := env.App.DB.Q().ListAudit(ctx, store.AuditFilter{ResourceID: a.ID, Action: "payment.expire"})
	if len(entries) != 1 || entries[0].ActorType != reqctx.ActorSystem {
		t.Fatalf("expiry audit = %+v", entries)
	}
}
