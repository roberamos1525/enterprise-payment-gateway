// Package merchant manages merchant accounts: onboarding, lifecycle status,
// fee schedules, API keys and webhook endpoints.
package merchant

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/audit"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

// Options configures the merchant service.
type Options struct {
	DefaultFee            FeeInput
	KeyRotationGrace      time.Duration // default validity of the old key after rotation
	SecretRotationGrace   time.Duration // validity of the old webhook secret after rotation
	MaxEndpoints          int
	WebhookURLPolicy      webhook.URLPolicy
	APIKeyTouchResolution time.Duration
}

// Service implements merchant management.
type Service struct {
	db    *store.DB
	audit *audit.Logger
	clock clock.Clock
	kr    *vault.Keyring
	opts  Options
}

// NewService builds the service.
func NewService(db *store.DB, al *audit.Logger, clk clock.Clock, kr *vault.Keyring, opts Options) *Service {
	if opts.MaxEndpoints <= 0 {
		opts.MaxEndpoints = 10
	}
	if opts.APIKeyTouchResolution <= 0 {
		opts.APIKeyTouchResolution = time.Minute
	}
	return &Service{db: db, audit: al, clock: clk, kr: kr, opts: opts}
}

// OnboardInput is the request to create a merchant.
type OnboardInput struct {
	Name            string    `json:"name"`
	Email           string    `json:"email"`
	Country         string    `json:"country"`
	DefaultCurrency string    `json:"default_currency"`
	Activate        bool      `json:"activate"`
	Fees            *FeeInput `json:"fees,omitempty"`
}

// FeeInput describes a fee schedule.
type FeeInput struct {
	PercentBps        int64      `json:"percent_bps"`
	FixedMinor        int64      `json:"fixed_minor"`
	RefundFixedMinor  int64      `json:"refund_fixed_minor"`
	ReturnFeeOnRefund bool       `json:"return_fee_on_refund"`
	EffectiveFrom     *time.Time `json:"effective_from,omitempty"`
}

func (f FeeInput) validate(v *apperr.Validator, prefix string) {
	v.Range(prefix+"percent_bps", f.PercentBps, 0, 10000)
	v.Range(prefix+"fixed_minor", f.FixedMinor, 0, 1_000_000)
	v.Range(prefix+"refund_fixed_minor", f.RefundFixedMinor, 0, 1_000_000)
}

// Onboard creates a merchant with its initial fee schedule.
func (s *Service) Onboard(ctx context.Context, in OnboardInput) (model.Merchant, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Country = strings.ToUpper(strings.TrimSpace(in.Country))
	var v apperr.Validator
	if v.Required("name", in.Name) {
		v.MaxLen("name", in.Name, 200)
	}
	if v.Required("email", in.Email) {
		v.Email("email", in.Email)
	}
	v.Country("country", in.Country)
	cur, err := money.NormalizeCurrency(in.DefaultCurrency)
	v.Check(err == nil, "default_currency", "must be a supported ISO 4217 currency")
	fees := s.opts.DefaultFee
	if in.Fees != nil {
		fees = *in.Fees
		fees.validate(&v, "fees.")
	}
	if err := v.Err(); err != nil {
		return model.Merchant{}, err
	}
	now := s.clock.Now()
	m := model.Merchant{
		ID: ids.New("mer"), Name: in.Name, Email: in.Email, Country: in.Country,
		DefaultCurrency: cur, Status: model.MerchantPending, CreatedAt: now, UpdatedAt: now,
	}
	if in.Activate {
		m.Status = model.MerchantActive
	}
	err = s.db.Tx(ctx, func(q *store.Queries) error {
		if err := q.InsertMerchant(ctx, m); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return apperr.New(apperr.CodeConflict, "a merchant with this email already exists")
			}
			return err
		}
		fs := feeSchedule(m.ID, fees, now, m.CreatedAt)
		if err := q.InsertFeeSchedule(ctx, fs); err != nil {
			return err
		}
		return s.audit.Record(ctx, q, audit.Entry{
			Action: "merchant.onboard", ResourceType: "merchant", ResourceID: m.ID, MerchantID: m.ID,
			Metadata: map[string]any{"status": m.Status, "country": m.Country, "fee_schedule_id": fs.ID},
		})
	})
	return m, err
}

func feeSchedule(merchantID string, f FeeInput, now, defaultEffective time.Time) model.FeeSchedule {
	eff := defaultEffective
	if f.EffectiveFrom != nil {
		eff = f.EffectiveFrom.UTC()
	}
	return model.FeeSchedule{
		ID: ids.New("fee"), MerchantID: merchantID, PercentBps: f.PercentBps, FixedMinor: f.FixedMinor,
		RefundFixedMinor: f.RefundFixedMinor, ReturnFeeOnRefund: f.ReturnFeeOnRefund,
		EffectiveFrom: eff, CreatedAt: now,
	}
}

// Get loads a merchant.
func (s *Service) Get(ctx context.Context, id string) (model.Merchant, error) {
	m, err := s.db.Q().GetMerchant(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return m, apperr.NotFound("merchant", id)
	}
	return m, err
}

// List pages through merchants.
func (s *Service) List(ctx context.Context, f store.MerchantFilter) ([]model.Merchant, bool, error) {
	return s.db.Q().ListMerchants(ctx, f)
}

// statusTransitions lists the allowed merchant lifecycle moves.
var statusTransitions = map[model.MerchantStatus][]model.MerchantStatus{
	model.MerchantPending:   {model.MerchantActive, model.MerchantClosed},
	model.MerchantActive:    {model.MerchantSuspended, model.MerchantClosed},
	model.MerchantSuspended: {model.MerchantActive, model.MerchantClosed},
}

// CanTransition reports whether a merchant may move from one status to another.
func CanTransition(from, to model.MerchantStatus) bool {
	for _, s := range statusTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// SetStatus moves a merchant through its lifecycle. Closing a merchant
// revokes all of its API keys.
func (s *Service) SetStatus(ctx context.Context, id string, to model.MerchantStatus, reason string) (model.Merchant, error) {
	var v apperr.Validator
	v.OneOf("status", string(to), string(model.MerchantActive), string(model.MerchantSuspended), string(model.MerchantClosed))
	v.MaxLen("reason", reason, 500)
	if err := v.Err(); err != nil {
		return model.Merchant{}, err
	}
	var out model.Merchant
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		m, err := q.GetMerchant(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return apperr.NotFound("merchant", id)
		}
		if err != nil {
			return err
		}
		if !CanTransition(m.Status, to) {
			return apperr.New(apperr.CodeInvalidState, "cannot move merchant from %q to %q", m.Status, to)
		}
		now := s.clock.Now()
		if err := q.UpdateMerchantStatus(ctx, id, to, now); err != nil {
			return err
		}
		revoked := 0
		if to == model.MerchantClosed {
			keys, err := q.ListAPIKeys(ctx, id)
			if err != nil {
				return err
			}
			for _, k := range keys {
				if k.Status == model.APIKeyActive {
					if err := q.RevokeAPIKey(ctx, k.ID, now); err != nil {
						return err
					}
					revoked++
				}
			}
		}
		from := m.Status
		m.Status, m.UpdatedAt = to, now
		out = m
		return s.audit.Record(ctx, q, audit.Entry{
			Action: "merchant.status_change", ResourceType: "merchant", ResourceID: id, MerchantID: id,
			Metadata: map[string]any{"from": from, "to": to, "reason": reason, "keys_revoked": revoked},
		})
	})
	return out, err
}

// UpdateProfileInput holds editable profile fields (nil = unchanged).
type UpdateProfileInput struct {
	Name  *string `json:"name,omitempty"`
	Email *string `json:"email,omitempty"`
}

// UpdateProfile edits a merchant's name or email.
func (s *Service) UpdateProfile(ctx context.Context, id string, in UpdateProfileInput) (model.Merchant, error) {
	var out model.Merchant
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		m, err := q.GetMerchant(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return apperr.NotFound("merchant", id)
		}
		if err != nil {
			return err
		}
		var v apperr.Validator
		if in.Name != nil {
			m.Name = strings.TrimSpace(*in.Name)
			if v.Required("name", m.Name) {
				v.MaxLen("name", m.Name, 200)
			}
		}
		if in.Email != nil {
			m.Email = strings.ToLower(strings.TrimSpace(*in.Email))
			v.Email("email", m.Email)
		}
		if err := v.Err(); err != nil {
			return err
		}
		m.UpdatedAt = s.clock.Now()
		if err := q.UpdateMerchantProfile(ctx, m); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return apperr.New(apperr.CodeConflict, "a merchant with this email already exists")
			}
			return err
		}
		out = m
		return s.audit.Record(ctx, q, audit.Entry{
			Action: "merchant.update", ResourceType: "merchant", ResourceID: id, MerchantID: id,
			Metadata: map[string]any{"name_changed": in.Name != nil, "email_changed": in.Email != nil},
		})
	})
	return out, err
}

// SetFeeSchedule adds a new fee schedule version for the merchant. Without
// effective_from it applies from now on; historic days keep their schedule.
func (s *Service) SetFeeSchedule(ctx context.Context, merchantID string, in FeeInput) (model.FeeSchedule, error) {
	var v apperr.Validator
	in.validate(&v, "")
	if err := v.Err(); err != nil {
		return model.FeeSchedule{}, err
	}
	now := s.clock.Now()
	fs := feeSchedule(merchantID, in, now, now)
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		if _, err := q.GetMerchant(ctx, merchantID); errors.Is(err, store.ErrNotFound) {
			return apperr.NotFound("merchant", merchantID)
		} else if err != nil {
			return err
		}
		if err := q.InsertFeeSchedule(ctx, fs); err != nil {
			return err
		}
		return s.audit.Record(ctx, q, audit.Entry{
			Action: "merchant.fee_schedule_set", ResourceType: "fee_schedule", ResourceID: fs.ID, MerchantID: merchantID,
			Metadata: map[string]any{"percent_bps": fs.PercentBps, "fixed_minor": fs.FixedMinor,
				"refund_fixed_minor": fs.RefundFixedMinor, "return_fee_on_refund": fs.ReturnFeeOnRefund,
				"effective_from": fs.EffectiveFrom},
		})
	})
	return fs, err
}

// FeeSchedules lists all fee schedule versions of a merchant.
func (s *Service) FeeSchedules(ctx context.Context, merchantID string) ([]model.FeeSchedule, error) {
	if _, err := s.Get(ctx, merchantID); err != nil {
		return nil, err
	}
	return s.db.Q().ListFeeSchedules(ctx, merchantID)
}
