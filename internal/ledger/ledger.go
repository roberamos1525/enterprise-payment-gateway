// Package ledger implements the gateway's internal double-entry ledger.
// Every money movement (capture, refund, settlement fee, payout) posts a
// balanced journal; reconciliation and settlement reports read from it.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

// Accounts. Asset accounts carry debit balances; liability and revenue
// accounts carry credit balances.
const (
	AcquirerReceivable = "acquirer_receivable" // asset: owed to us by the acquirer
	MerchantPayable    = "merchant_payable"    // liability: owed by us to the merchant
	FeeRevenue         = "fee_revenue"         // revenue: processing fees earned
	PayoutsPending     = "payouts_pending"     // liability: payouts scheduled, not yet sent
	Cash               = "cash"                // asset: the gateway's settlement bank account
)

// Journal kinds.
const (
	KindCapture    = "capture"
	KindRefund     = "refund"
	KindFee        = "settlement_fee"
	KindPayout     = "payout"
	KindPayoutPaid = "payout_paid"
)

var debitNormal = map[string]bool{AcquirerReceivable: true, Cash: true}

// Errors.
var (
	ErrUnbalanced = errors.New("ledger: journal is not balanced")
	ErrInvalid    = errors.New("ledger: invalid journal")
	ErrDuplicate  = errors.New("ledger: journal already posted")
)

// Ledger posts journals.
type Ledger struct {
	clock clock.Clock
}

// New builds a ledger.
func New(clk clock.Clock) *Ledger { return &Ledger{clock: clk} }

// Validate checks that a set of entries is well formed and balanced.
func Validate(entries []model.JournalEntry) error {
	if len(entries) < 2 {
		return fmt.Errorf("%w: need at least two entries", ErrInvalid)
	}
	var debits, credits int64
	for _, e := range entries {
		if e.Account == "" {
			return fmt.Errorf("%w: empty account", ErrInvalid)
		}
		if e.Debit < 0 || e.Credit < 0 || (e.Debit == 0) == (e.Credit == 0) {
			return fmt.Errorf("%w: entry on %s must have exactly one positive side", ErrInvalid, e.Account)
		}
		var ok bool
		if debits, ok = money.AddInt64(debits, e.Debit); !ok {
			return money.ErrOverflow
		}
		if credits, ok = money.AddInt64(credits, e.Credit); !ok {
			return money.ErrOverflow
		}
	}
	if debits != credits {
		return fmt.Errorf("%w: debits %d != credits %d", ErrUnbalanced, debits, credits)
	}
	return nil
}

// Post validates and writes a journal. Posting the same (kind, ref) twice
// returns ErrDuplicate, so callers can safely retry.
func (l *Ledger) Post(ctx context.Context, q *store.Queries, kind, ref, merchantID, currency string, entries ...model.JournalEntry) (model.Journal, error) {
	if err := Validate(entries); err != nil {
		return model.Journal{}, err
	}
	if kind == "" || ref == "" || merchantID == "" || currency == "" {
		return model.Journal{}, fmt.Errorf("%w: kind, ref, merchant and currency are required", ErrInvalid)
	}
	j := model.Journal{
		ID: ids.New("jnl"), Kind: kind, Ref: ref, MerchantID: merchantID, Currency: currency,
		CreatedAt: l.clock.Now(), Entries: entries,
	}
	if err := q.InsertJournal(ctx, j); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return model.Journal{}, ErrDuplicate
		}
		return model.Journal{}, err
	}
	return j, nil
}

// Move is a two-legged posting of amount from the credit account to the
// debit account. Zero amounts are a no-op; negative amounts swap the legs.
func (l *Ledger) Move(ctx context.Context, q *store.Queries, kind, ref, merchantID, currency, debitAcct, creditAcct string, amount int64) error {
	if amount == 0 {
		return nil
	}
	if amount < 0 {
		debitAcct, creditAcct, amount = creditAcct, debitAcct, -amount
	}
	_, err := l.Post(ctx, q, kind, ref, merchantID, currency,
		model.JournalEntry{Account: debitAcct, Debit: amount},
		model.JournalEntry{Account: creditAcct, Credit: amount})
	return err
}

// RecordCapture posts a capture: the acquirer owes us, we owe the merchant.
func (l *Ledger) RecordCapture(ctx context.Context, q *store.Queries, ref, merchantID, currency string, amount int64) error {
	return l.Move(ctx, q, KindCapture, ref, merchantID, currency, AcquirerReceivable, MerchantPayable, amount)
}

// RecordRefund posts a refund, reversing (part of) a capture.
func (l *Ledger) RecordRefund(ctx context.Context, q *store.Queries, ref, merchantID, currency string, amount int64) error {
	return l.Move(ctx, q, KindRefund, ref, merchantID, currency, MerchantPayable, AcquirerReceivable, amount)
}

// Balance is a signed account balance in its normal direction.
type Balance struct {
	Account  string `json:"account"`
	Currency string `json:"currency"`
	Debits   int64  `json:"debits"`
	Credits  int64  `json:"credits"`
	Balance  int64  `json:"balance"`
}

// Balances returns a merchant's account balances, each signed by the
// account's normal side (positive = normal).
func Balances(ctx context.Context, q *store.Queries, merchantID string) ([]Balance, error) {
	rows, err := q.AccountBalances(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	out := make([]Balance, 0, len(rows))
	for _, r := range rows {
		b := Balance{Account: r.Account, Currency: r.Currency, Debits: r.Debits, Credits: r.Credits}
		if debitNormal[r.Account] {
			b.Balance = r.Debits - r.Credits
		} else {
			b.Balance = r.Credits - r.Debits
		}
		out = append(out, b)
	}
	return out, nil
}

// TrialBalanceResult reports whether the whole ledger balances per currency.
type TrialBalanceResult struct {
	Balanced   bool                `json:"balanced"`
	Currencies map[string][2]int64 `json:"currencies"`
}

// TrialBalance sums every entry per currency; debits must equal credits.
func TrialBalance(ctx context.Context, q *store.Queries) (TrialBalanceResult, error) {
	totals, err := q.TrialBalance(ctx)
	if err != nil {
		return TrialBalanceResult{}, err
	}
	res := TrialBalanceResult{Balanced: true, Currencies: totals}
	for _, t := range totals {
		if t[0] != t[1] {
			res.Balanced = false
		}
	}
	return res, nil
}
