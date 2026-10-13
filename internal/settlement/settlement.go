// Package settlement runs the daily settlement batch: for each merchant it
// collects the day's unsettled captures and refunds, applies the fee
// schedule in force, books fees and payouts in the ledger, and schedules a
// payout of the net amount.
package settlement

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/audit"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/ledger"
	"github.com/robertramos/enterprise-payment-gateway/internal/metrics"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

// Batch statuses.
const (
	StatusPayoutScheduled = "payout_scheduled"
	StatusNoPayout        = "no_payout" // net <= 0; carried as a negative merchant balance
)

// Options configures settlement.
type Options struct {
	Rounding        money.Rounding
	PayoutDelayDays int // payout date = settlement date + delay
}

// Service runs settlements.
type Service struct {
	db      *store.DB
	ledger  *ledger.Ledger
	outbox  *webhook.Outbox
	audit   *audit.Logger
	clock   clock.Clock
	metrics *metrics.Gateway
	log     *slog.Logger
	opts    Options
}

// NewService builds the service.
func NewService(db *store.DB, l *ledger.Ledger, o *webhook.Outbox, al *audit.Logger, clk clock.Clock,
	m *metrics.Gateway, log *slog.Logger, opts Options) *Service {
	if opts.PayoutDelayDays < 0 {
		opts.PayoutDelayDays = 0
	}
	return &Service{db: db, ledger: l, outbox: o, audit: al, clock: clk, metrics: m, log: log, opts: opts}
}

// RunResult summarises a settlement run.
type RunResult struct {
	Date             string                  `json:"settlement_date"`
	MerchantsSettled int                     `json:"merchants_settled"`
	MerchantsSkipped int                     `json:"merchants_without_activity"`
	Batches          []model.SettlementBatch `json:"batches"`
	PayoutsScheduled int                     `json:"payouts_scheduled"`
	MerchantsFailed  []string                `json:"merchants_failed,omitempty"`
}

// RunInput selects what to settle.
type RunInput struct {
	Day        time.Time
	MerchantID string // empty = all merchants
	// AllowOpenDay permits settling the current, not yet finished day; items
	// arriving later that day are settled by a subsequent run as an extra batch.
	AllowOpenDay bool
}

// Run settles a UTC day for one merchant or all merchants. By default only
// days that have fully elapsed can be settled. Re-running is safe: items
// already in a batch are never settled twice.
func (s *Service) Run(ctx context.Context, in RunInput) (RunResult, error) {
	day := clock.StartOfDay(in.Day)
	merchantID := in.MerchantID
	res := RunResult{Date: day.Format(clock.DateLayout), Batches: []model.SettlementBatch{}}
	now := s.clock.Now()
	if day.After(now) {
		return res, apperr.New(apperr.CodeInvalidRequest, "settlement date %s is in the future", res.Date)
	}
	if day.AddDate(0, 0, 1).After(now) && !in.AllowOpenDay {
		return res, apperr.New(apperr.CodeInvalidRequest,
			"settlement date %s has not ended yet (set allow_open_day to settle it early)", res.Date)
	}
	var merchants []string
	if merchantID != "" {
		if _, err := s.db.Q().GetMerchant(ctx, merchantID); errors.Is(err, store.ErrNotFound) {
			return res, apperr.NotFound("merchant", merchantID)
		} else if err != nil {
			return res, err
		}
		merchants = []string{merchantID}
	} else {
		var err error
		if merchants, err = s.db.Q().ListMerchantIDs(ctx); err != nil {
			return res, err
		}
	}
	for _, mid := range merchants {
		batches, err := s.settleMerchant(ctx, mid, day)
		if err != nil {
			s.log.Error("settlement failed", "merchant_id", mid, "date", res.Date, "error", err)
			res.MerchantsFailed = append(res.MerchantsFailed, mid)
			s.count("error")
			continue
		}
		if len(batches) == 0 {
			res.MerchantsSkipped++
			continue
		}
		res.MerchantsSettled++
		for _, b := range batches {
			if b.PayoutID != "" {
				res.PayoutsScheduled++
			}
			s.count(b.Status)
		}
		res.Batches = append(res.Batches, batches...)
	}
	return res, nil
}

func (s *Service) count(outcome string) {
	if s.metrics != nil {
		s.metrics.SettlementRuns.With(outcome).Inc()
	}
}

type currencyGroup struct {
	items []model.SettlementItem
	batch model.SettlementBatch
}

// settleMerchant builds and persists one batch per currency for a merchant/day.
func (s *Service) settleMerchant(ctx context.Context, merchantID string, day time.Time) ([]model.SettlementBatch, error) {
	var out []model.SettlementBatch
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		from, to := day, day.AddDate(0, 0, 1)
		caps, err := q.UnsettledCaptures(ctx, merchantID, from, to)
		if err != nil {
			return err
		}
		refs, err := q.UnsettledRefunds(ctx, merchantID, from, to)
		if err != nil {
			return err
		}
		if len(caps) == 0 && len(refs) == 0 {
			return nil
		}
		fs, err := q.EffectiveFeeSchedule(ctx, merchantID, to.Add(-time.Nanosecond))
		if err != nil {
			return fmt.Errorf("settlement: no fee schedule for %s: %w", merchantID, err)
		}
		groups, err := s.buildGroups(merchantID, day, fs, caps, refs)
		if err != nil {
			return err
		}
		for _, cur := range sortedCurrencies(groups) {
			b, err := s.persistGroup(ctx, q, groups[cur], day)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
		return nil
	})
	return out, err
}

func sortedCurrencies(m map[string]*currencyGroup) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// buildGroups computes per-item fees and per-currency totals.
func (s *Service) buildGroups(merchantID string, day time.Time, fs model.FeeSchedule, caps []model.SettleableCapture, refs []model.SettleableRefund) (map[string]*currencyGroup, error) {
	now := s.clock.Now()
	groups := map[string]*currencyGroup{}
	group := func(cur string) *currencyGroup {
		g, ok := groups[cur]
		if !ok {
			g = &currencyGroup{batch: model.SettlementBatch{
				ID: ids.New("stl"), MerchantID: merchantID, Date: day.Format(clock.DateLayout), Currency: cur,
				FeeScheduleID: fs.ID, CreatedAt: now,
			}}
			groups[cur] = g
		}
		return g
	}
	for _, c := range caps {
		fee, err := CaptureFee(fs, c.Amount, s.opts.Rounding)
		if err != nil {
			return nil, err
		}
		g := group(c.Currency)
		g.items = append(g.items, model.SettlementItem{BatchID: g.batch.ID, Kind: model.ItemCapture, RefID: c.PaymentID,
			PaymentID: c.PaymentID, Amount: c.Amount, Fee: fee, OccurredAt: c.CapturedAt})
		g.batch.GrossCaptured += c.Amount
		g.batch.Fees += fee
		g.batch.CaptureCount++
	}
	for _, r := range refs {
		fee, err := RefundFee(fs, r.Amount, s.opts.Rounding)
		if err != nil {
			return nil, err
		}
		g := group(r.Currency)
		g.items = append(g.items, model.SettlementItem{BatchID: g.batch.ID, Kind: model.ItemRefund, RefID: r.RefundID,
			PaymentID: r.PaymentID, Amount: r.Amount, Fee: fee, OccurredAt: r.CreatedAt})
		g.batch.Refunded += r.Amount
		g.batch.Fees += fee
		g.batch.RefundCount++
	}
	for _, g := range groups {
		g.batch.Net = g.batch.GrossCaptured - g.batch.Refunded - g.batch.Fees
		g.batch.Status = StatusNoPayout
		if g.batch.Net > 0 {
			g.batch.Status = StatusPayoutScheduled
		}
	}
	return groups, nil
}

// CaptureFee is percent + fixed on a capture.
func CaptureFee(fs model.FeeSchedule, amount int64, r money.Rounding) (int64, error) {
	return money.Fee(amount, fs.PercentBps, fs.FixedMinor, r)
}

// RefundFee charges the fixed refund fee and, when the schedule returns
// fees on refunds, credits back the percentage fee on the refunded amount
// (a negative contribution).
func RefundFee(fs model.FeeSchedule, amount int64, r money.Rounding) (int64, error) {
	fee := fs.RefundFixedMinor
	if fs.ReturnFeeOnRefund {
		pct, err := money.ApplyBps(amount, fs.PercentBps, r)
		if err != nil {
			return 0, err
		}
		fee -= pct
	}
	return fee, nil
}

func (s *Service) persistGroup(ctx context.Context, q *store.Queries, g *currencyGroup, day time.Time) (model.SettlementBatch, error) {
	b := g.batch
	if err := q.InsertBatch(ctx, b, g.items); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return b, apperr.New(apperr.CodeConflict, "settlement items were settled concurrently; retry")
		}
		return b, err
	}
	if err := s.ledger.Move(ctx, q, ledger.KindFee, b.ID, b.MerchantID, b.Currency,
		ledger.MerchantPayable, ledger.FeeRevenue, b.Fees); err != nil {
		return b, err
	}
	if err := s.audit.Record(ctx, q, audit.Entry{Action: "settlement.create", ResourceType: "settlement_batch",
		ResourceID: b.ID, MerchantID: b.MerchantID, Metadata: map[string]any{"date": b.Date, "currency": b.Currency,
			"gross": b.GrossCaptured, "refunded": b.Refunded, "fees": b.Fees, "net": b.Net}}); err != nil {
		return b, err
	}
	if b.Net > 0 {
		p := model.Payout{
			ID: ids.New("po"), BatchID: b.ID, MerchantID: b.MerchantID, Amount: b.Net, Currency: b.Currency,
			Status: "scheduled", ScheduledFor: day.AddDate(0, 0, s.opts.PayoutDelayDays).Format(clock.DateLayout),
			CreatedAt: s.clock.Now(),
		}
		if err := q.InsertPayout(ctx, p); err != nil {
			return b, err
		}
		if err := s.ledger.Move(ctx, q, ledger.KindPayout, p.ID, b.MerchantID, b.Currency,
			ledger.MerchantPayable, ledger.PayoutsPending, p.Amount); err != nil {
			return b, err
		}
		if _, err := s.outbox.Publish(ctx, q, b.MerchantID, webhook.EventPayoutScheduled, p.ID, p); err != nil {
			return b, err
		}
		b.PayoutID = p.ID
	}
	if _, err := s.outbox.Publish(ctx, q, b.MerchantID, webhook.EventSettlementCreated, b.ID, b); err != nil {
		return b, err
	}
	return b, nil
}

// PayDuePayouts marks scheduled payouts due on or before today as paid
// (simulating the bank transfer) and books them against cash.
func (s *Service) PayDuePayouts(ctx context.Context) (int, error) {
	today := s.clock.Now().Format(clock.DateLayout)
	due, err := s.db.Q().DuePayouts(ctx, today)
	if err != nil {
		return 0, err
	}
	paid := 0
	for _, p := range due {
		marked := false
		err := s.db.Tx(ctx, func(q *store.Queries) error {
			now := s.clock.Now()
			if err := q.MarkPayoutPaid(ctx, p.ID, now); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil // already paid by a concurrent run
				}
				return err
			}
			if err := s.ledger.Move(ctx, q, ledger.KindPayoutPaid, p.ID, p.MerchantID, p.Currency,
				ledger.PayoutsPending, ledger.Cash, p.Amount); err != nil {
				return err
			}
			p.Status, p.PaidAt = "paid", &now
			if _, err := s.outbox.Publish(ctx, q, p.MerchantID, webhook.EventPayoutPaid, p.ID, p); err != nil {
				return err
			}
			marked = true
			return s.audit.Record(ctx, q, audit.Entry{Action: "payout.paid", ResourceType: "payout", ResourceID: p.ID,
				MerchantID: p.MerchantID, Metadata: map[string]any{"amount": p.Amount, "currency": p.Currency}})
		})
		if err != nil {
			return paid, err
		}
		if marked {
			paid++
		}
	}
	return paid, nil
}
