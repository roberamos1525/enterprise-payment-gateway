package fraud

import (
	"context"
	"errors"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/audit"
	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/reqctx"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

// Blocklist manages the persisted BIN blocklist.
type Blocklist struct {
	db    *store.DB
	audit *audit.Logger
	clock clock.Clock
}

// NewBlocklist builds the blocklist service.
func NewBlocklist(db *store.DB, al *audit.Logger, clk clock.Clock) *Blocklist {
	return &Blocklist{db: db, audit: al, clock: clk}
}

// Check implements BlocklistChecker.
func (b *Blocklist) Check(ctx context.Context, bin string) (bool, string, error) {
	e, err := b.db.Q().GetBlockedBIN(ctx, bin)
	if errors.Is(err, store.ErrNotFound) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, e.Reason, nil
}

func validateBIN(bin, reason string) error {
	var v apperr.Validator
	v.Check(len(bin) == 6 && card.IsDigits(bin), "bin", "must be exactly 6 digits")
	if v.Required("reason", reason) {
		v.MaxLen("reason", reason, 200)
	}
	return v.Err()
}

// Add blocks a BIN.
func (b *Blocklist) Add(ctx context.Context, bin, reason string) (model.BlockedBIN, error) {
	if err := validateBIN(bin, reason); err != nil {
		return model.BlockedBIN{}, err
	}
	actor := reqctx.ActorFrom(ctx)
	e := model.BlockedBIN{BIN: bin, Reason: reason, CreatedBy: actor.Type + ":" + actor.ID, CreatedAt: b.clock.Now()}
	err := b.db.Tx(ctx, func(q *store.Queries) error {
		if err := q.InsertBlockedBIN(ctx, e); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return apperr.New(apperr.CodeConflict, "BIN %s is already blocklisted", bin)
			}
			return err
		}
		return b.audit.Record(ctx, q, audit.Entry{
			Action: "fraud.bin_block", ResourceType: "bin", ResourceID: bin, Metadata: map[string]any{"reason": reason},
		})
	})
	return e, err
}

// Remove unblocks a BIN.
func (b *Blocklist) Remove(ctx context.Context, bin string) error {
	return b.db.Tx(ctx, func(q *store.Queries) error {
		if err := q.DeleteBlockedBIN(ctx, bin); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return apperr.NotFound("blocklisted BIN", bin)
			}
			return err
		}
		return b.audit.Record(ctx, q, audit.Entry{Action: "fraud.bin_unblock", ResourceType: "bin", ResourceID: bin})
	})
}

// List returns all blocked BINs.
func (b *Blocklist) List(ctx context.Context) ([]model.BlockedBIN, error) {
	return b.db.Q().ListBlockedBINs(ctx)
}

// Seed inserts configured default BINs that are not yet present. It is
// idempotent and runs at startup.
func (b *Blocklist) Seed(ctx context.Context, bins []string) error {
	ctx = reqctx.WithActor(ctx, reqctx.System("startup"))
	for _, bin := range bins {
		if _, err := b.Add(ctx, bin, "seeded from configuration"); err != nil && !apperr.Is(err, apperr.CodeConflict) {
			return err
		}
	}
	return nil
}
