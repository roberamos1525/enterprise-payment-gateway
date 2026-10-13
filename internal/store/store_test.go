package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

var t0 = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedMerchant(t *testing.T, db *DB, id string) {
	t.Helper()
	err := db.Q().InsertMerchant(context.Background(), model.Merchant{ID: id, Name: id, Email: id + "@x.test",
		Country: "US", DefaultCurrency: "USD", Status: model.MerchantActive, CreatedAt: t0, UpdatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	ctx := context.Background()
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := db.MigrationVersion(ctx)
	if err != nil || v != 1 {
		t.Fatalf("version = %d, %v", v, err)
	}
	db.Close()
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	if v, _ := db.MigrationVersion(ctx); v != 1 {
		t.Fatalf("version after reopen = %d", v)
	}
	if _, err := Open(ctx, "bad?path.db"); err == nil {
		t.Fatal("paths with '?' must be rejected")
	}
}

func TestMerchantUniqueEmailAndPaging(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	for _, id := range []string{"mer_a", "mer_b", "mer_c"} {
		seedMerchant(t, db, id)
	}
	err := db.Q().InsertMerchant(ctx, model.Merchant{ID: "mer_d", Name: "d", Email: "mer_a@x.test", Country: "US",
		DefaultCurrency: "USD", Status: model.MerchantPending, CreatedAt: t0, UpdatedAt: t0})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate email: %v", err)
	}
	page1, more, err := db.Q().ListMerchants(ctx, MerchantFilter{Page: Page{Limit: 2}})
	if err != nil || len(page1) != 2 || !more || page1[0].ID != "mer_c" {
		t.Fatalf("page1 = %v more=%v err=%v", page1, more, err)
	}
	page2, more, err := db.Q().ListMerchants(ctx, MerchantFilter{Page: Page{Limit: 2, StartingAfter: page1[1].ID}})
	if err != nil || len(page2) != 1 || more || page2[0].ID != "mer_a" {
		t.Fatalf("page2 = %v more=%v err=%v", page2, more, err)
	}
	if _, err := db.Q().GetMerchant(ctx, "mer_zzz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing merchant: %v", err)
	}
	if err := db.Q().UpdateMerchantStatus(ctx, "mer_zzz", model.MerchantClosed, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

func TestPaymentRoundTripAndOptimisticLock(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	seedMerchant(t, db, "mer_p")
	exp := t0.Add(time.Hour)
	p := &model.PaymentIntent{
		ID: "pi_1", MerchantID: "mer_p", Amount: 1234, Currency: "EUR", Status: model.StatusCreated,
		CaptureMethod: model.CaptureManual, Metadata: map[string]string{"k": "v"}, Version: 1,
		CreatedAt: t0, UpdatedAt: t0, AuthExpiresAt: &exp,
		Risk: model.Risk{Score: 30, Action: "allow", Reasons: []string{"a", "b"}},
		Card: &model.CardSummary{Token: "tok_1", Brand: "visa", BIN: "424242", Last4: "4242", Country: "US", Fingerprint: "fp"},
	}
	if err := db.Q().InsertPayment(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := db.Q().GetPayment(ctx, "pi_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount != 1234 || got.Metadata["k"] != "v" || got.Card.Last4 != "4242" || len(got.Risk.Reasons) != 2 ||
		!got.AuthExpiresAt.Equal(exp) || got.CapturedAt != nil {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	stale := *got
	got.Status = model.StatusAuthorized
	if err := db.Q().UpdatePayment(ctx, got); err != nil || got.Version != 2 {
		t.Fatalf("update: %v version=%d", err, got.Version)
	}
	stale.Status = model.StatusCanceled
	if err := db.Q().UpdatePayment(ctx, &stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update should conflict, got %v", err)
	}
	counts, err := db.Q().CountPaymentsByStatus(ctx)
	if err != nil || counts[model.StatusAuthorized] != 1 {
		t.Fatalf("counts = %v %v", counts, err)
	}
	ids, err := db.Q().ListExpiredAuthorizations(ctx, t0.Add(2*time.Hour), 10)
	if err != nil || len(ids) != 1 {
		t.Fatalf("expired = %v %v", ids, err)
	}
}

func TestTransactionRollback(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	boom := errors.New("boom")
	err := db.Tx(ctx, func(q *Queries) error {
		if err := q.InsertMerchant(ctx, model.Merchant{ID: "mer_rb", Name: "x", Email: "rb@x.test", Country: "US",
			DefaultCurrency: "USD", Status: model.MerchantActive, CreatedAt: t0, UpdatedAt: t0}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := db.Q().GetMerchant(ctx, "mer_rb"); !errors.Is(err, ErrNotFound) {
		t.Fatal("rolled back insert is visible")
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if _, err := db.Q().InsertAudit(ctx, model.AuditEntry{ID: "aud_1", CreatedAt: t0, ActorType: "admin", ActorID: "ops",
		Action: "x", ResourceType: "r", ResourceID: "1", PrevHash: "0", Hash: "h1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `UPDATE audit_logs SET action = 'y'`); err == nil ||
		!strings.Contains(err.Error(), "append-only") {
		t.Fatalf("update allowed: %v", err)
	}
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM audit_logs`); err == nil {
		t.Fatal("delete allowed")
	}
	h, err := db.Q().LastAuditHash(ctx)
	if err != nil || h != "h1" {
		t.Fatalf("last hash = %q %v", h, err)
	}
}

func TestIdempotencyLocking(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	rec := model.IdempotencyRecord{MerchantID: "m", Key: "k", Method: "POST", Path: "/p", Fingerprint: "f",
		CreatedAt: t0, ExpiresAt: t0.Add(time.Hour), LockedUntil: t0.Add(time.Minute)}
	if err := db.Q().UpsertIdempotencyLock(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := db.Q().UpsertIdempotencyLock(ctx, rec); !errors.Is(err, ErrConflict) {
		t.Fatalf("second lock: %v", err)
	}
	if err := db.Q().RelockIdempotency(ctx, "m", "k", t0, t0.Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("relock while held: %v", err)
	}
	if err := db.Q().RelockIdempotency(ctx, "m", "k", t0.Add(2*time.Minute), t0.Add(3*time.Minute)); err != nil {
		t.Fatalf("relock after lapse: %v", err)
	}
	if err := db.Q().CompleteIdempotency(ctx, "m", "k", 201, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	got, err := db.Q().GetIdempotency(ctx, "m", "k")
	if err != nil || !got.Completed || got.ResponseStatus != 201 || string(got.ResponseBody) != `{"ok":true}` {
		t.Fatalf("record = %+v %v", got, err)
	}
	// An expired record is replaced by a new lock.
	rec.CreatedAt = t0.Add(2 * time.Hour)
	rec.ExpiresAt = rec.CreatedAt.Add(time.Hour)
	if err := db.Q().UpsertIdempotencyLock(ctx, rec); err != nil {
		t.Fatalf("replace expired: %v", err)
	}
	n, err := db.Q().PurgeIdempotency(ctx, t0.Add(10*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purge = %d %v", n, err)
	}
}

func TestLedgerJournalUniqueness(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	j := model.Journal{ID: "jnl_1", Kind: "capture", Ref: "acq_1", MerchantID: "m", Currency: "USD", CreatedAt: t0,
		Entries: []model.JournalEntry{{Account: "a", Debit: 5}, {Account: "b", Credit: 5}}}
	if err := db.Q().InsertJournal(ctx, j); err != nil {
		t.Fatal(err)
	}
	j.ID = "jnl_2"
	if err := db.Q().InsertJournal(ctx, j); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate (kind, ref): %v", err)
	}
	js, err := db.Q().ListJournals(ctx, "m", []string{"capture", "refund"}, t0, t0.Add(time.Second))
	if err != nil || len(js) != 1 || len(js[0].Entries) != 2 {
		t.Fatalf("journals = %+v %v", js, err)
	}
	tb, err := db.Q().TrialBalance(ctx)
	if err != nil || tb["USD"] != [2]int64{5, 5} {
		t.Fatalf("trial balance = %v %v", tb, err)
	}
}

func TestDeliveryClaimLeases(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	seedMerchant(t, db, "mer_w")
	q := db.Q()
	if err := q.InsertEndpoint(ctx, model.WebhookEndpoint{ID: "we_1", MerchantID: "mer_w", URL: "https://x", Events: []string{"*"},
		Status: model.EndpointEnabled, SecretSealed: "s", CreatedAt: t0, UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertEvent(ctx, model.Event{ID: "evt_1", MerchantID: "mer_w", Type: "t", ResourceID: "r",
		Payload: []byte(`{}`), CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertDelivery(ctx, model.Delivery{ID: "whd_1", EventID: "evt_1", EndpointID: "we_1", MerchantID: "mer_w",
		Status: model.DeliveryPending, NextAttemptAt: t0, CreatedAt: t0, UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	var first, second []model.Delivery
	_ = db.Tx(ctx, func(q *Queries) (err error) { first, err = q.ClaimDueDeliveries(ctx, t0, time.Minute, 10); return })
	_ = db.Tx(ctx, func(q *Queries) (err error) { second, err = q.ClaimDueDeliveries(ctx, t0, time.Minute, 10); return })
	if len(first) != 1 || len(second) != 0 {
		t.Fatalf("lease not honoured: %d then %d", len(first), len(second))
	}
	// After the lease expires the delivery can be claimed again.
	var third []model.Delivery
	_ = db.Tx(ctx, func(q *Queries) (err error) {
		third, err = q.ClaimDueDeliveries(ctx, t0.Add(2*time.Minute), time.Minute, 10)
		return
	})
	if len(third) != 1 {
		t.Fatalf("expired lease not reclaimable: %d", len(third))
	}
}
