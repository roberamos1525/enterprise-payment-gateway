package settlement

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

// Report is the detailed statement of one settlement batch.
type Report struct {
	Batch  model.SettlementBatch  `json:"batch"`
	Items  []model.SettlementItem `json:"items"`
	Payout *model.Payout          `json:"payout,omitempty"`
}

// Batch returns a merchant's batch ("" merchant = any, for admins).
func (s *Service) Batch(ctx context.Context, merchantID, id string) (model.SettlementBatch, error) {
	b, err := s.db.Q().GetBatch(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && merchantID != "" && b.MerchantID != merchantID) {
		return b, apperr.NotFound("settlement batch", id)
	}
	return b, err
}

// List pages through batches.
func (s *Service) List(ctx context.Context, f store.BatchFilter) ([]model.SettlementBatch, bool, error) {
	return s.db.Q().ListBatches(ctx, f)
}

// Payouts pages through a merchant's payouts.
func (s *Service) Payouts(ctx context.Context, merchantID string, page store.Page) ([]model.Payout, bool, error) {
	return s.db.Q().ListPayouts(ctx, merchantID, page)
}

// Report builds the statement for a batch.
func (s *Service) Report(ctx context.Context, merchantID, id string) (Report, error) {
	b, err := s.Batch(ctx, merchantID, id)
	if err != nil {
		return Report{}, err
	}
	items, err := s.db.Q().ListBatchItems(ctx, id)
	if err != nil {
		return Report{}, err
	}
	r := Report{Batch: b, Items: items}
	if b.PayoutID != "" {
		p, err := s.db.Q().GetPayout(ctx, b.PayoutID)
		if err != nil {
			return Report{}, err
		}
		r.Payout = &p
	}
	return r, nil
}

// WriteCSV renders the report as CSV: one row per item followed by a
// summary row. Amounts are minor units plus a decimal rendering.
func (r Report) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	cur := r.Batch.Currency
	if err := cw.Write([]string{"batch_id", "kind", "ref_id", "payment_intent_id", "occurred_at", "currency",
		"amount_minor", "fee_minor", "net_minor", "amount", "fee", "net"}); err != nil {
		return err
	}
	for _, it := range r.Items {
		net := it.Amount - it.Fee
		if it.Kind == model.ItemRefund {
			net = -it.Amount - it.Fee
		}
		if err := cw.Write([]string{r.Batch.ID, it.Kind, it.RefID, it.PaymentID, it.OccurredAt.Format(time.RFC3339),
			cur, i64(it.Amount), i64(it.Fee), i64(net), money.FormatMinor(it.Amount, cur), money.FormatMinor(it.Fee, cur),
			money.FormatMinor(net, cur)}); err != nil {
			return err
		}
	}
	b := r.Batch
	if err := cw.Write([]string{b.ID, "TOTAL", "", "", b.Date, cur, i64(b.GrossCaptured - b.Refunded), i64(b.Fees),
		i64(b.Net), money.FormatMinor(b.GrossCaptured-b.Refunded, cur), money.FormatMinor(b.Fees, cur),
		money.FormatMinor(b.Net, cur)}); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

func i64(n int64) string { return strconv.FormatInt(n, 10) }

// DaySummary aggregates all batches of a day per currency.
type DaySummary struct {
	Date       string            `json:"settlement_date"`
	Currencies []CurrencySummary `json:"currencies"`
}

// CurrencySummary totals one currency for a day.
type CurrencySummary struct {
	Currency      string `json:"currency"`
	Batches       int    `json:"batches"`
	GrossCaptured int64  `json:"gross_captured"`
	Refunded      int64  `json:"refunded"`
	Fees          int64  `json:"fees"`
	Net           int64  `json:"net"`
}

// Summary totals every batch for a day (optionally one merchant).
func (s *Service) Summary(ctx context.Context, merchantID, date string) (DaySummary, error) {
	sum := DaySummary{Date: date, Currencies: []CurrencySummary{}}
	idx := map[string]int{}
	page := store.Page{Limit: 100}
	for {
		batches, more, err := s.db.Q().ListBatches(ctx, store.BatchFilter{MerchantID: merchantID, Date: date, Page: page})
		if err != nil {
			return sum, err
		}
		for _, b := range batches {
			i, ok := idx[b.Currency]
			if !ok {
				i = len(sum.Currencies)
				idx[b.Currency] = i
				sum.Currencies = append(sum.Currencies, CurrencySummary{Currency: b.Currency})
			}
			c := &sum.Currencies[i]
			c.Batches++
			c.GrossCaptured += b.GrossCaptured
			c.Refunded += b.Refunded
			c.Fees += b.Fees
			c.Net += b.Net
		}
		if !more || len(batches) == 0 {
			return sum, nil
		}
		page.StartingAfter = batches[len(batches)-1].ID
	}
}
