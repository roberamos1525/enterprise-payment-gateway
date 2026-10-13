package ledger

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

func TestValidate(t *testing.T) {
	ok := []model.JournalEntry{{Account: "a", Debit: 10}, {Account: "b", Credit: 7}, {Account: "c", Credit: 3}}
	if err := Validate(ok); err != nil {
		t.Fatalf("valid journal rejected: %v", err)
	}
	cases := []struct {
		name    string
		entries []model.JournalEntry
		want    error
	}{
		{"single", []model.JournalEntry{{Account: "a", Debit: 1}}, ErrInvalid},
		{"unbalanced", []model.JournalEntry{{Account: "a", Debit: 2}, {Account: "b", Credit: 1}}, ErrUnbalanced},
		{"both sides", []model.JournalEntry{{Account: "a", Debit: 1, Credit: 1}, {Account: "b", Credit: 0, Debit: 0}}, ErrInvalid},
		{"negative", []model.JournalEntry{{Account: "a", Debit: -1}, {Account: "b", Credit: -1}}, ErrInvalid},
		{"no account", []model.JournalEntry{{Debit: 1}, {Account: "b", Credit: 1}}, ErrInvalid},
		{"overflow", []model.JournalEntry{{Account: "a", Debit: math.MaxInt64}, {Account: "b", Debit: 1},
			{Account: "c", Credit: 1}}, money.ErrOverflow},
	}
	for _, c := range cases {
		if err := Validate(c.entries); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestPostingsAndBalances(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l := New(clock.NewFake(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)))
	q := db.Q()
	if err := l.RecordCapture(ctx, q, "acq_c1", "mer_1", "USD", 10_000); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordRefund(ctx, q, "acq_r1", "mer_1", "USD", 2_500); err != nil {
		t.Fatal(err)
	}
	// Posting the same business event twice is detected.
	if err := l.RecordCapture(ctx, q, "acq_c1", "mer_1", "USD", 10_000); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate capture: %v", err)
	}
	// Negative fees (fee returns) reverse the legs.
	if err := l.Move(ctx, q, KindFee, "stl_1", "mer_1", "USD", MerchantPayable, FeeRevenue, -40); err != nil {
		t.Fatal(err)
	}
	if err := l.Move(ctx, q, KindFee, "stl_2", "mer_1", "USD", MerchantPayable, FeeRevenue, 0); err != nil {
		t.Fatal("zero move should be a no-op")
	}
	if _, err := l.Post(ctx, q, "", "x", "mer_1", "USD", model.JournalEntry{Account: "a", Debit: 1},
		model.JournalEntry{Account: "b", Credit: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing kind: %v", err)
	}
	bals, err := Balances(ctx, q, "mer_1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, b := range bals {
		got[b.Account] = b.Balance
	}
	// receivable: 10000 - 2500; payable: 10000 - 2500 + 40 (fee returned); fee revenue: -40.
	if got[AcquirerReceivable] != 7_500 || got[MerchantPayable] != 7_540 || got[FeeRevenue] != -40 {
		t.Fatalf("balances = %v", got)
	}
	tb, err := TrialBalance(ctx, q)
	if err != nil || !tb.Balanced {
		t.Fatalf("trial balance = %+v %v", tb, err)
	}
}
