// Package reconcile compares the gateway's internal ledger with the
// acquirer's daily settlement file and reports every discrepancy.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/acquirer"
	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/audit"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/ledger"
	"github.com/robertramos/enterprise-payment-gateway/internal/metrics"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

// Discrepancy kinds.
const (
	MissingAtAcquirer   = "missing_at_acquirer"
	MissingInternally   = "missing_internally"
	AmountMismatch      = "amount_mismatch"
	CurrencyMismatch    = "currency_mismatch"
	TypeMismatch        = "type_mismatch"
	DuplicateAtAcquirer = "duplicate_at_acquirer"
)

// Sources of the acquirer file.
const (
	SourceSimulator = "simulator"
	SourceUpload    = "upload"
)

// Service runs reconciliations.
type Service struct {
	db      *store.DB
	acq     *acquirer.Acquirer
	audit   *audit.Logger
	clock   clock.Clock
	metrics *metrics.Gateway
}

// NewService builds the service.
func NewService(db *store.DB, acq *acquirer.Acquirer, al *audit.Logger, clk clock.Clock, m *metrics.Gateway) *Service {
	return &Service{db: db, acq: acq, audit: al, clock: clk, metrics: m}
}

// internalRecord is a capture or refund as booked in the ledger.
type internalRecord struct {
	ref        string
	recordType string
	merchantID string
	amount     int64
	currency   string
}

// internalRecords reads the day's capture and refund journals. Journals are
// keyed by acquirer reference, which is the join key with the file.
func (s *Service) internalRecords(ctx context.Context, day time.Time) ([]internalRecord, error) {
	from := clock.StartOfDay(day)
	journals, err := s.db.Q().ListJournals(ctx, "", []string{ledger.KindCapture, ledger.KindRefund}, from, from.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	out := make([]internalRecord, 0, len(journals))
	for _, j := range journals {
		var amt int64
		for _, e := range j.Entries {
			amt += e.Debit
		}
		rt := acquirer.RecordCapture
		if j.Kind == ledger.KindRefund {
			rt = acquirer.RecordRefund
		}
		out = append(out, internalRecord{ref: j.Ref, recordType: rt, merchantID: j.MerchantID, amount: amt, currency: j.Currency})
	}
	return out, nil
}

// RunFromSimulator reconciles a day against the simulated acquirer's file.
func (s *Service) RunFromSimulator(ctx context.Context, day time.Time) (model.ReconciliationRun, error) {
	rows, err := s.acq.SettlementFile(ctx, day)
	if err != nil {
		return model.ReconciliationRun{}, err
	}
	return s.Run(ctx, day, rows, SourceSimulator)
}

// RunFromCSV reconciles a day against an uploaded settlement file.
func (s *Service) RunFromCSV(ctx context.Context, day time.Time, r io.Reader) (model.ReconciliationRun, error) {
	rows, err := acquirer.ParseCSV(r)
	if err != nil {
		var pe *acquirer.ParseError
		if errors.As(err, &pe) {
			return model.ReconciliationRun{}, apperr.New(apperr.CodeValidation, "settlement file rejected: %s", pe.Error())
		}
		return model.ReconciliationRun{}, err
	}
	return s.Run(ctx, day, rows, SourceUpload)
}

// Run matches the ledger against file rows and persists the result.
func (s *Service) Run(ctx context.Context, day time.Time, rows []acquirer.FileRow, source string) (model.ReconciliationRun, error) {
	day = clock.StartOfDay(day)
	internal, err := s.internalRecords(ctx, day)
	if err != nil {
		return model.ReconciliationRun{}, err
	}
	run := Match(internal, rows)
	run.ID = ids.New("rec")
	run.Date = day.Format(clock.DateLayout)
	run.Source = source
	run.CreatedAt = s.clock.Now()
	for i := range run.Discrepancies {
		run.Discrepancies[i].ID = ids.New("dsc")
		run.Discrepancies[i].RunID = run.ID
	}
	err = s.db.Tx(ctx, func(q *store.Queries) error {
		if err := q.InsertReconciliation(ctx, run); err != nil {
			return err
		}
		return s.audit.Record(ctx, q, audit.Entry{Action: "reconciliation.run", ResourceType: "reconciliation",
			ResourceID: run.ID, Metadata: map[string]any{"date": run.Date, "source": source, "status": run.Status,
				"matched": run.MatchedCount, "discrepancies": run.DiscrepancyCount}})
	})
	if err != nil {
		return model.ReconciliationRun{}, err
	}
	if s.metrics != nil {
		s.metrics.Reconciliations.With(run.Status).Inc()
	}
	return run, nil
}

// Match is the pure matching algorithm (exported for testing and reuse).
func Match(internal []internalRecord, rows []acquirer.FileRow) model.ReconciliationRun {
	run := model.ReconciliationRun{InternalCount: len(internal), AcquirerCount: len(rows), Discrepancies: []model.Discrepancy{}}
	byRef := map[string][]acquirer.FileRow{}
	for _, r := range rows {
		byRef[r.Ref] = append(byRef[r.Ref], r)
	}
	totals := map[string]*model.CurrencyTotals{}
	total := func(cur string) *model.CurrencyTotals {
		t, ok := totals[cur]
		if !ok {
			t = &model.CurrencyTotals{Currency: cur}
			totals[cur] = t
		}
		return t
	}
	add := func(d model.Discrepancy) { run.Discrepancies = append(run.Discrepancies, d) }
	seen := map[string]bool{}
	for _, in := range internal {
		signed := in.amount
		if in.recordType == acquirer.RecordRefund {
			signed = -signed
		}
		total(in.currency).Internal += signed
		ia := in.amount
		matches := byRef[in.ref]
		seen[in.ref] = true
		if len(matches) == 0 {
			add(model.Discrepancy{Kind: MissingAtAcquirer, AcquirerRef: in.ref, MerchantID: in.merchantID,
				InternalAmount: &ia, Currency: in.currency,
				Detail: fmt.Sprintf("%s booked internally but absent from the acquirer file", in.recordType)})
			continue
		}
		row := matches[0]
		aa := row.Amount
		if len(matches) > 1 {
			add(model.Discrepancy{Kind: DuplicateAtAcquirer, AcquirerRef: in.ref, MerchantID: in.merchantID,
				InternalAmount: &ia, AcquirerAmount: &aa, Currency: in.currency,
				Detail: fmt.Sprintf("acquirer file lists this reference %d times", len(matches))})
		}
		switch {
		case row.RecordType != in.recordType:
			add(model.Discrepancy{Kind: TypeMismatch, AcquirerRef: in.ref, MerchantID: in.merchantID,
				InternalAmount: &ia, AcquirerAmount: &aa, Currency: in.currency,
				Detail: fmt.Sprintf("internal %s, acquirer %s", in.recordType, row.RecordType)})
		case row.Currency != in.currency:
			add(model.Discrepancy{Kind: CurrencyMismatch, AcquirerRef: in.ref, MerchantID: in.merchantID,
				InternalAmount: &ia, AcquirerAmount: &aa, Currency: in.currency,
				Detail: fmt.Sprintf("internal %s, acquirer %s", in.currency, row.Currency)})
		case row.Amount != in.amount:
			add(model.Discrepancy{Kind: AmountMismatch, AcquirerRef: in.ref, MerchantID: in.merchantID,
				InternalAmount: &ia, AcquirerAmount: &aa, Currency: in.currency,
				Detail: fmt.Sprintf("internal %s, acquirer %s (difference %s)", money.FormatMinor(in.amount, in.currency),
					money.FormatMinor(row.Amount, in.currency), money.FormatMinor(row.Amount-in.amount, in.currency))})
		default:
			if len(matches) == 1 {
				run.MatchedCount++
			}
		}
	}
	for _, r := range rows {
		signed := r.Amount
		if r.RecordType == acquirer.RecordRefund {
			signed = -signed
		}
		total(r.Currency).Acquirer += signed
		if seen[r.Ref] {
			continue
		}
		seen[r.Ref] = true
		aa := r.Amount
		add(model.Discrepancy{Kind: MissingInternally, AcquirerRef: r.Ref, MerchantID: r.MerchantID,
			AcquirerAmount: &aa, Currency: r.Currency,
			Detail: fmt.Sprintf("%s in acquirer file has no matching ledger journal", r.RecordType)})
	}
	sort.SliceStable(run.Discrepancies, func(i, j int) bool {
		if run.Discrepancies[i].Kind != run.Discrepancies[j].Kind {
			return run.Discrepancies[i].Kind < run.Discrepancies[j].Kind
		}
		return run.Discrepancies[i].AcquirerRef < run.Discrepancies[j].AcquirerRef
	})
	for _, cur := range sortedKeys(totals) {
		t := totals[cur]
		t.Diff = t.Acquirer - t.Internal
		run.Totals = append(run.Totals, *t)
	}
	run.DiscrepancyCount = len(run.Discrepancies)
	run.Status = "balanced"
	if run.DiscrepancyCount > 0 {
		run.Status = "discrepancies"
	}
	return run
}

func sortedKeys(m map[string]*model.CurrencyTotals) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Get loads a run with its discrepancies.
func (s *Service) Get(ctx context.Context, id string) (model.ReconciliationRun, error) {
	r, err := s.db.Q().GetReconciliation(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return r, apperr.NotFound("reconciliation", id)
	}
	return r, err
}

// List pages through runs.
func (s *Service) List(ctx context.Context, page store.Page) ([]model.ReconciliationRun, bool, error) {
	return s.db.Q().ListReconciliations(ctx, page)
}
