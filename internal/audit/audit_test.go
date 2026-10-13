package audit

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/reqctx"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRecordChainsAndVerifies(t *testing.T) {
	db := openDB(t)
	clk := clock.NewFake(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	l := NewLogger(clk)
	ctx := reqctx.WithActor(context.Background(), reqctx.Actor{Type: reqctx.ActorMerchant, ID: "key_1", MerchantID: "mer_1"})
	ctx = reqctx.WithRequestID(ctx, "req_1")
	ctx = reqctx.WithClientIP(ctx, "192.0.2.10")
	for i := 0; i < 3; i++ {
		clk.Advance(time.Second)
		if err := l.Record(ctx, db.Q(), Entry{Action: "payment.capture", ResourceType: "payment_intent",
			ResourceID: "pi_1", Metadata: map[string]any{"n": i}}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := db.Q().ListAudit(context.Background(), store.AuditFilter{MerchantID: "mer_1"})
	if err != nil || len(entries) != 3 {
		t.Fatalf("entries = %d %v", len(entries), err)
	}
	if entries[0].PrevHash != GenesisHash || entries[1].PrevHash != entries[0].Hash {
		t.Fatal("chain links broken")
	}
	e := entries[0]
	if e.ActorType != "merchant" || e.ActorID != "key_1" || e.RequestID != "req_1" || e.IP != "192.0.2.10" {
		t.Fatalf("context not captured: %+v", e)
	}
	res, err := Verify(context.Background(), db.Q())
	if err != nil || !res.Valid || res.Entries != 3 || res.HeadHash != entries[2].Hash {
		t.Fatalf("verify = %+v %v", res, err)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	db := openDB(t)
	l := NewLogger(clock.NewFake(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)))
	ctx := context.Background()
	if err := l.Record(ctx, db.Q(), Entry{Action: "a", ResourceType: "r", ResourceID: "1"}); err != nil {
		t.Fatal(err)
	}
	// Simulate an attacker who bypasses the triggers by inserting a forged
	// row whose content does not match its hash.
	forged := model.AuditEntry{ID: "aud_forged", CreatedAt: time.Now().UTC(), ActorType: "admin", ActorID: "x",
		Action: "b", ResourceType: "r", ResourceID: "2", Metadata: json.RawMessage(`{}`)}
	forged.PrevHash, _ = db.Q().LastAuditHash(ctx)
	forged.Hash = strings.Repeat("a", 64)
	if _, err := db.Q().InsertAudit(ctx, forged); err != nil {
		t.Fatal(err)
	}
	res, err := Verify(ctx, db.Q())
	if err != nil || res.Valid || res.BrokenAtSeq != 2 || res.Reason != "content hash mismatch" {
		t.Fatalf("verify = %+v %v", res, err)
	}
	// A row that does not link to its predecessor is also detected.
	db2 := openDB(t)
	if err := l.Record(ctx, db2.Q(), Entry{Action: "a", ResourceType: "r", ResourceID: "1"}); err != nil {
		t.Fatal(err)
	}
	unlinked := forged
	unlinked.PrevHash = GenesisHash
	unlinked.Hash = Hash(unlinked)
	if _, err := db2.Q().InsertAudit(ctx, unlinked); err != nil {
		t.Fatal(err)
	}
	res, _ = Verify(ctx, db2.Q())
	if res.Valid || res.Reason != "prev_hash does not match predecessor" {
		t.Fatalf("unlinked = %+v", res)
	}
}

func TestMetadataIsRedacted(t *testing.T) {
	db := openDB(t)
	l := NewLogger(clock.Real{})
	ctx := context.Background()
	if err := l.Record(ctx, db.Q(), Entry{Action: "x", ResourceType: "r", ResourceID: "1",
		Metadata: map[string]any{"note": "customer said 4242 4242 4242 4242"}}); err != nil {
		t.Fatal(err)
	}
	e, _ := db.Q().ListAudit(ctx, store.AuditFilter{})
	if strings.Contains(string(e[0].Metadata), "4242 4242 4242 4242") || !strings.Contains(string(e[0].Metadata), "424242******4242") {
		t.Fatalf("metadata = %s", e[0].Metadata)
	}
	if reqctx.ActorFrom(ctx).ID != "anonymous" || e[0].ActorType != "system" {
		t.Fatalf("default actor wrong: %+v", e[0])
	}
}

func TestHashIsUnambiguous(t *testing.T) {
	a := model.AuditEntry{ID: "ab", Action: "c", Metadata: json.RawMessage(`{}`)}
	b := model.AuditEntry{ID: "a", Action: "bc", Metadata: json.RawMessage(`{}`)}
	if Hash(a) == Hash(b) {
		t.Fatal("length-prefixing should prevent field-boundary collisions")
	}
}
