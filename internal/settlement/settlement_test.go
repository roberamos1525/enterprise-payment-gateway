package settlement_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/internal/ledger"
	"github.com/robertramos/enterprise-payment-gateway/internal/merchant"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
	"github.com/robertramos/enterprise-payment-gateway/internal/payment"
	"github.com/robertramos/enterprise-payment-gateway/internal/settlement"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/tests/testenv"
)

func TestFeeFunctions(t *testing.T) {
	fs := model.FeeSchedule{PercentBps: 290, FixedMinor: 30, RefundFixedMinor: 15}
	if f, _ := settlement.CaptureFee(fs, 10_000, money.HalfEven); f != 320 {
		t.Fatalf("capture fee = %d", f)
	}
	if f, _ := settlement.RefundFee(fs, 10_000, money.HalfEven); f != 15 {
		t.Fatalf("refund fee = %d", f)
	}
	fs.ReturnFeeOnRefund = true
	if f, _ := settlement.RefundFee(fs, 10_000, money.HalfEven); f != 15-290 {
		t.Fatalf("refund fee with return = %d", f)
	}
}

type flow struct {
	env *testenv.Env
	m   model.Merchant
}

func (f flow) capture(t *testing.T, amount int64, currency string) *model.PaymentIntent {
	t.Helper()
	p, err := f.env.App.Payments.Create(context.Background(), f.m, payment.CreateInput{Amount: amount, Currency: currency,
		CaptureMethod: "automatic", Confirm: true,
		PaymentMethod: &payment.AuthorizeInput{Card: &card.Input{Number: "5555555555554444", ExpMonth: 1, ExpYear: 2031, CVC: "321"}}})
	if err != nil || p.Status != model.StatusCaptured {
		t.Fatalf("capture: %v %+v", err, p)
	}
	return p
}

func (f flow) refund(t *testing.T, p *model.PaymentIntent, amount int64) {
	t.Helper()
	if _, _, err := f.env.App.Payments.Refund(context.Background(), f.m, p.ID, payment.RefundInput{Amount: &amount}); err != nil {
		t.Fatal(err)
	}
}

func TestDailyBatchPerCurrencyWithFees(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	m, err := env.App.Merchants.Onboard(ctx, merchant.OnboardInput{Name: "Multi", Email: "multi@x.test", Country: "DE",
		DefaultCurrency: "EUR", Activate: true, Fees: &merchant.FeeInput{PercentBps: 200, FixedMinor: 20,
			RefundFixedMinor: 5, ReturnFeeOnRefund: true}})
	if err != nil {
		t.Fatal(err)
	}
	f := flow{env: env, m: m}
	p1 := f.capture(t, 10_000, "EUR")
	f.capture(t, 5_000, "EUR")
	f.capture(t, 3_000, "USD")
	f.refund(t, p1, 4_000)
	day := env.Clock.Now()

	if _, err := env.App.Settlements.Run(ctx, settlement.RunInput{Day: day}); !apperr.Is(err, apperr.CodeInvalidRequest) {
		t.Fatalf("open day: %v", err)
	}
	if _, err := env.App.Settlements.Run(ctx, settlement.RunInput{Day: day.AddDate(0, 0, 2)}); !apperr.Is(err, apperr.CodeInvalidRequest) {
		t.Fatalf("future day: %v", err)
	}
	env.Clock.Advance(24 * time.Hour)
	res, err := env.App.Settlements.Run(ctx, settlement.RunInput{Day: day, MerchantID: m.ID})
	if err != nil || len(res.Batches) != 2 || res.PayoutsScheduled != 2 {
		t.Fatalf("run = %+v %v", res, err)
	}
	eur, usd := res.Batches[0], res.Batches[1]
	if eur.Currency != "EUR" || usd.Currency != "USD" {
		t.Fatalf("batch order: %s %s", eur.Currency, usd.Currency)
	}
	// EUR fees: 10000 -> 200+20=220, 5000 -> 100+20=120, refund 4000 -> 5-80=-75. Total 265.
	if eur.GrossCaptured != 15_000 || eur.Refunded != 4_000 || eur.Fees != 265 || eur.Net != 10_735 ||
		eur.CaptureCount != 2 || eur.RefundCount != 1 {
		t.Fatalf("eur batch = %+v", eur)
	}
	if usd.Fees != 80 || usd.Net != 2_920 {
		t.Fatalf("usd batch = %+v", usd)
	}
	rep, err := env.App.Settlements.Report(ctx, m.ID, eur.ID)
	if err != nil || len(rep.Items) != 3 || rep.Payout == nil || rep.Payout.Amount != eur.Net ||
		rep.Payout.ScheduledFor != day.AddDate(0, 0, 1).Format("2006-01-02") {
		t.Fatalf("report = %+v %v", rep, err)
	}
	var buf bytes.Buffer
	if err := rep.WriteCSV(&buf); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(&buf).ReadAll()
	if err != nil || len(records) != 5 || records[4][1] != "TOTAL" || records[4][8] != "10735" {
		t.Fatalf("csv = %v %v", records, err)
	}
	if _, err := env.App.Settlements.Report(ctx, "mer_other", eur.ID); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("cross-merchant report: %v", err)
	}
	sum, err := env.App.Settlements.Summary(ctx, "", day.Format("2006-01-02"))
	if err != nil || len(sum.Currencies) != 2 {
		t.Fatalf("summary = %+v %v", sum, err)
	}
	// Ledger: fees booked to revenue, net moved to payouts_pending.
	bals, _ := ledger.Balances(ctx, env.App.DB.Q(), m.ID)
	got := map[string]int64{}
	for _, b := range bals {
		got[b.Account+"/"+b.Currency] = b.Balance
	}
	if got["fee_revenue/EUR"] != 265 || got["payouts_pending/EUR"] != 10_735 || got["merchant_payable/EUR"] != 0 {
		t.Fatalf("balances = %v", got)
	}
}

func TestLateItemsFormSupplementaryBatchAndPayouts(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	f := flow{env: env, m: env.Onboard("Late").Merchant}
	day := env.Clock.Now()
	f.capture(t, 1_000, "USD")
	res, err := env.App.Settlements.Run(ctx, settlement.RunInput{Day: day, AllowOpenDay: true})
	if err != nil || len(res.Batches) != 1 {
		t.Fatalf("early run = %+v %v", res, err)
	}
	if n, _ := env.App.Settlements.PayDuePayouts(ctx); n != 0 {
		t.Fatalf("paid before due: %d", n)
	}
	p := f.capture(t, 2_000, "USD")
	f.refund(t, p, 2_000)
	env.Clock.Advance(14 * time.Hour)
	res, err = env.App.Settlements.Run(ctx, settlement.RunInput{Day: day})
	if err != nil || len(res.Batches) != 1 {
		t.Fatalf("second run = %+v %v", res, err)
	}
	b := res.Batches[0]
	// Capture 2000 fee 88, refund 2000 -> net = 2000-2000-88 < 0: no payout.
	if b.Status != settlement.StatusNoPayout || b.Net != -88 || b.PayoutID != "" {
		t.Fatalf("late batch = %+v", b)
	}
	list, _, err := env.App.Settlements.List(ctx, store.BatchFilter{MerchantID: f.m.ID})
	if err != nil || len(list) != 2 {
		t.Fatalf("batches = %d %v", len(list), err)
	}
	if res, _ := env.App.Settlements.Run(ctx, settlement.RunInput{Day: day}); len(res.Batches) != 0 || res.MerchantsSkipped != 1 {
		t.Fatalf("rerun = %+v", res)
	}
	// Payouts are paid once their date (the next day) arrives.
	if n, err := env.App.Settlements.PayDuePayouts(ctx); n != 1 || err != nil {
		t.Fatalf("paid = %d %v", n, err)
	}
	if n, _ := env.App.Settlements.PayDuePayouts(ctx); n != 0 {
		t.Fatal("payout paid twice")
	}
	payouts, _, _ := env.App.Settlements.Payouts(ctx, f.m.ID, store.Page{})
	if len(payouts) != 1 || payouts[0].Status != "paid" || payouts[0].PaidAt == nil {
		t.Fatalf("payouts = %+v", payouts)
	}
	tb, _ := ledger.TrialBalance(ctx, env.App.DB.Q())
	if !tb.Balanced {
		t.Fatalf("trial balance = %+v", tb)
	}
}

func TestFeeScheduleVersioning(t *testing.T) {
	env := testenv.New(t, func(c *config.Config) { c.Fees.PercentBps, c.Fees.FixedMinor = 100, 0 })
	ctx := context.Background()
	f := flow{env: env, m: env.Onboard("Versioned").Merchant}
	day1 := env.Clock.Now()
	f.capture(t, 10_000, "USD")
	env.Clock.Advance(24 * time.Hour)
	if _, err := env.App.Merchants.SetFeeSchedule(ctx, f.m.ID, merchant.FeeInput{PercentBps: 500}); err != nil {
		t.Fatal(err)
	}
	day2 := env.Clock.Now()
	f.capture(t, 10_000, "USD")
	env.Clock.Advance(24 * time.Hour)
	r1, _ := env.App.Settlements.Run(ctx, settlement.RunInput{Day: day1})
	r2, _ := env.App.Settlements.Run(ctx, settlement.RunInput{Day: day2})
	if r1.Batches[0].Fees != 100 || r2.Batches[0].Fees != 500 {
		t.Fatalf("fees day1=%d day2=%d", r1.Batches[0].Fees, r2.Batches[0].Fees)
	}
	if _, err := env.App.Settlements.Run(ctx, settlement.RunInput{Day: day1, MerchantID: "mer_missing"}); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("unknown merchant: %v", err)
	}
}
