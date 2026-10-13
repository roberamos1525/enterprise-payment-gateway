// Package audit writes the append-only, hash-chained audit log. Each entry
// stores the hash of its predecessor, so any edit or deletion of historic
// rows (which SQLite triggers already reject) is detectable by Verify.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/reqctx"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

// GenesisHash is the prev_hash of the first entry.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Entry describes an auditable action. Actor, request id and IP are taken
// from the context.
type Entry struct {
	Action       string
	ResourceType string
	ResourceID   string
	MerchantID   string
	Metadata     map[string]any
}

// Logger appends audit entries.
type Logger struct {
	clock clock.Clock
}

// NewLogger builds a logger.
func NewLogger(clk clock.Clock) *Logger { return &Logger{clock: clk} }

// Record appends an entry inside the caller's transaction, so the audit
// record commits atomically with the change it describes.
func (l *Logger) Record(ctx context.Context, q *store.Queries, e Entry) error {
	actor := reqctx.ActorFrom(ctx)
	md := []byte("{}")
	if len(e.Metadata) > 0 {
		var err error
		if md, err = json.Marshal(e.Metadata); err != nil {
			return fmt.Errorf("audit: metadata: %w", err)
		}
		// Defense in depth: no PAN may ever reach the audit log.
		md = []byte(card.RedactPANs(string(md)))
	}
	prev, err := q.LastAuditHash(ctx)
	if err != nil {
		return fmt.Errorf("audit: read chain head: %w", err)
	}
	if prev == "" {
		prev = GenesisHash
	}
	merchantID := e.MerchantID
	if merchantID == "" {
		merchantID = actor.MerchantID
	}
	entry := model.AuditEntry{
		ID:           ids.New("aud"),
		CreatedAt:    l.clock.Now(),
		ActorType:    actor.Type,
		ActorID:      actor.ID,
		Action:       e.Action,
		ResourceType: e.ResourceType,
		ResourceID:   e.ResourceID,
		MerchantID:   merchantID,
		RequestID:    reqctx.RequestID(ctx),
		IP:           reqctx.ClientIP(ctx),
		Metadata:     md,
		PrevHash:     prev,
	}
	entry.Hash = Hash(entry)
	if _, err := q.InsertAudit(ctx, entry); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// Hash computes an entry's chained hash over its prev_hash and content.
// Fields are length-prefixed so no two different entries serialise equally.
func Hash(e model.AuditEntry) string {
	h := sha256.New()
	for _, f := range []string{
		e.PrevHash, e.ID, strconv.FormatInt(e.CreatedAt.UnixNano(), 10), e.ActorType, e.ActorID, e.Action,
		e.ResourceType, e.ResourceID, e.MerchantID, e.RequestID, e.IP, string(e.Metadata),
	} {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{':'})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyResult reports the outcome of a chain verification.
type VerifyResult struct {
	Valid       bool   `json:"valid"`
	Entries     int64  `json:"entries_checked"`
	BrokenAtSeq int64  `json:"broken_at_seq,omitempty"`
	Reason      string `json:"reason,omitempty"`
	HeadHash    string `json:"head_hash,omitempty"`
}

// Verify walks the whole chain, recomputing every hash.
func Verify(ctx context.Context, q *store.Queries) (VerifyResult, error) {
	res := VerifyResult{Valid: true}
	prev := GenesisHash
	var after int64
	for {
		batch, err := q.ListAudit(ctx, store.AuditFilter{AfterSeq: after, Limit: 500})
		if err != nil {
			return res, err
		}
		if len(batch) == 0 {
			res.HeadHash = prev
			return res, nil
		}
		for _, e := range batch {
			res.Entries++
			if e.PrevHash != prev {
				return broken(res, e.Seq, "prev_hash does not match predecessor"), nil
			}
			if Hash(e) != e.Hash {
				return broken(res, e.Seq, "content hash mismatch"), nil
			}
			prev = e.Hash
			after = e.Seq
		}
	}
}

func broken(res VerifyResult, seq int64, reason string) VerifyResult {
	res.Valid = false
	res.BrokenAtSeq = seq
	res.Reason = reason
	return res
}
