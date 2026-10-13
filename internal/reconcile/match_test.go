package reconcile

import (
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/acquirer"
)

var ts = time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

func row(rt, ref string, amount int64, cur string) acquirer.FileRow {
	return acquirer.FileRow{RecordType: rt, Ref: ref, MerchantID: "mer_1", Amount: amount, Currency: cur, Time: ts}
}

func rec(rt, ref string, amount int64, cur string) internalRecord {
	return internalRecord{ref: ref, recordType: rt, merchantID: "mer_1", amount: amount, currency: cur}
}

func TestMatchBalanced(t *testing.T) {
	internal := []internalRecord{rec(acquirer.RecordCapture, "a", 1000, "USD"), rec(acquirer.RecordRefund, "b", 200, "USD")}
	rows := []acquirer.FileRow{row(acquirer.RecordRefund, "b", 200, "USD"), row(acquirer.RecordCapture, "a", 1000, "USD")}
	run := Match(internal, rows)
	if run.Status != "balanced" || run.MatchedCount != 2 || run.DiscrepancyCount != 0 {
		t.Fatalf("run = %+v", run)
	}
	if len(run.Totals) != 1 || run.Totals[0].Internal != 800 || run.Totals[0].Acquirer != 800 || run.Totals[0].Diff != 0 {
		t.Fatalf("totals = %+v", run.Totals)
	}
}

func TestMatchEveryDiscrepancyKind(t *testing.T) {
	internal := []internalRecord{
		rec(acquirer.RecordCapture, "missing", 100, "USD"),
		rec(acquirer.RecordCapture, "amount", 100, "USD"),
		rec(acquirer.RecordCapture, "currency", 100, "USD"),
		rec(acquirer.RecordCapture, "type", 100, "USD"),
		rec(acquirer.RecordCapture, "dup", 100, "USD"),
		rec(acquirer.RecordCapture, "ok", 100, "USD"),
	}
	rows := []acquirer.FileRow{
		row(acquirer.RecordCapture, "amount", 150, "USD"),
		row(acquirer.RecordCapture, "currency", 100, "EUR"),
		row(acquirer.RecordRefund, "type", 100, "USD"),
		row(acquirer.RecordCapture, "dup", 100, "USD"),
		row(acquirer.RecordCapture, "dup", 100, "USD"),
		row(acquirer.RecordCapture, "ok", 100, "USD"),
		row(acquirer.RecordCapture, "phantom", 999, "USD"),
	}
	run := Match(internal, rows)
	kinds := map[string]string{}
	for _, d := range run.Discrepancies {
		kinds[d.AcquirerRef] = d.Kind
	}
	want := map[string]string{
		"missing": MissingAtAcquirer, "amount": AmountMismatch, "currency": CurrencyMismatch,
		"type": TypeMismatch, "dup": DuplicateAtAcquirer, "phantom": MissingInternally,
	}
	for ref, k := range want {
		if kinds[ref] != k {
			t.Errorf("%s: kind %q, want %q", ref, kinds[ref], k)
		}
	}
	if run.MatchedCount != 1 || run.Status != "discrepancies" || run.DiscrepancyCount != 6 {
		t.Fatalf("run = %+v", run)
	}
	for i := 1; i < len(run.Discrepancies); i++ {
		if run.Discrepancies[i-1].Kind > run.Discrepancies[i].Kind {
			t.Fatal("discrepancies must be sorted by kind")
		}
	}
	for _, d := range run.Discrepancies {
		if d.Kind == AmountMismatch && (*d.InternalAmount != 100 || *d.AcquirerAmount != 150 ||
			d.Detail != "internal 1.00, acquirer 1.50 (difference 0.50)") {
			t.Fatalf("amount detail = %+v", d)
		}
		if d.Kind == MissingInternally && (d.InternalAmount != nil || *d.AcquirerAmount != 999) {
			t.Fatalf("phantom = %+v", d)
		}
	}
	// Per-currency totals expose the money impact.
	var usd, eur int64
	for _, tot := range run.Totals {
		switch tot.Currency {
		case "USD":
			usd = tot.Diff
		case "EUR":
			eur = tot.Diff
		}
	}
	// internal USD = 600; acquirer USD = 150 - 100 + 100 + 100 + 100 + 999 = 1349.
	if usd != 749 || eur != 100 {
		t.Fatalf("diffs usd=%d eur=%d (%+v)", usd, eur, run.Totals)
	}
}

func TestMatchEmpty(t *testing.T) {
	run := Match(nil, nil)
	if run.Status != "balanced" || run.Discrepancies == nil || len(run.Totals) != 0 {
		t.Fatalf("empty run = %+v", run)
	}
}
