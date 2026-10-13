package payment

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/acquirer"
	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/audit"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/fraud"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/ledger"
	"github.com/robertramos/enterprise-payment-gateway/internal/metrics"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
	"github.com/robertramos/enterprise-payment-gateway/internal/view"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

// Options configures the payment service.
type Options struct {
	AuthorizationTTL time.Duration // how long an uncaptured authorization is held
	MaxAmountMinor   int64         // per-payment ceiling in minor units
	AcquirerTimeout  time.Duration // bound on each simulated network call
}

// Deps are the collaborators of the payment service.
type Deps struct {
	DB       *store.DB
	Vault    *vault.Service
	Acquirer *acquirer.Acquirer
	Fraud    *fraud.Engine
	Ledger   *ledger.Ledger
	Outbox   *webhook.Outbox
	Audit    *audit.Logger
	Clock    clock.Clock
	Metrics  *metrics.Gateway
	Log      *slog.Logger
}

// Service implements the payment lifecycle.
type Service struct {
	Deps
	opts  Options
	locks *keyedMutex
}

// NewService builds the service.
func NewService(d Deps, opts Options) *Service {
	if opts.AuthorizationTTL <= 0 {
		opts.AuthorizationTTL = 7 * 24 * time.Hour
	}
	if opts.MaxAmountMinor <= 0 {
		opts.MaxAmountMinor = 100_000_000
	}
	if opts.AcquirerTimeout <= 0 {
		opts.AcquirerTimeout = 15 * time.Second
	}
	return &Service{Deps: d, opts: opts, locks: newKeyedMutex()}
}

// pendingEvent is an outbox event to publish with a state change.
type pendingEvent struct {
	typ        string
	resourceID string
	data       any
}

// change describes one atomic state change of a payment.
type change struct {
	p       *model.PaymentIntent
	from    model.PaymentStatus
	action  Action
	amount  int64
	detail  string
	events  []pendingEvent
	extra   func(q *store.Queries) error
	auditMD map[string]any
}

// commit persists a change: payment row (optimistically locked), history,
// extra writes (ledger, refunds), outbox events and the audit record — all
// in one transaction.
func (s *Service) commit(ctx context.Context, c change) error {
	if err := CheckTransition(c.from, c.action, c.p.Status); err != nil {
		return err
	}
	now := s.Clock.Now()
	c.p.UpdatedAt = now
	err := s.DB.Tx(ctx, func(q *store.Queries) error {
		if err := q.UpdatePayment(ctx, c.p); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return apperr.New(apperr.CodeConflict, "payment intent was modified concurrently; retry")
			}
			return err
		}
		if err := q.InsertHistory(ctx, model.PaymentHistory{
			ID: ids.New("ph"), PaymentID: c.p.ID, Action: string(c.action), FromStatus: string(c.from),
			ToStatus: string(c.p.Status), Amount: c.amount, Detail: c.detail, CreatedAt: now,
		}); err != nil {
			return err
		}
		if c.extra != nil {
			if err := c.extra(q); err != nil {
				return err
			}
		}
		for _, ev := range c.events {
			if _, err := s.Outbox.Publish(ctx, q, c.p.MerchantID, ev.typ, ev.resourceID, ev.data); err != nil {
				return err
			}
		}
		md := map[string]any{"from": c.from, "to": c.p.Status, "amount": c.amount}
		for k, v := range c.auditMD {
			md[k] = v
		}
		return s.Audit.Record(ctx, q, audit.Entry{
			Action: "payment." + string(c.action), ResourceType: "payment_intent", ResourceID: c.p.ID,
			MerchantID: c.p.MerchantID, Metadata: md,
		})
	})
	if err == nil && s.Metrics != nil {
		s.Metrics.PaymentOps.With(string(c.action), string(c.p.Status)).Inc()
	}
	return err
}

func paymentEvent(typ string, p *model.PaymentIntent) pendingEvent {
	return pendingEvent{typ: typ, resourceID: p.ID, data: view.PaymentIntent(p)}
}

// load fetches a payment owned by merchantID (other merchants' payments are
// reported as not found).
func (s *Service) load(ctx context.Context, merchantID, id string) (*model.PaymentIntent, error) {
	p, err := s.DB.Q().GetPayment(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && merchantID != "" && p.MerchantID != merchantID) {
		return nil, apperr.NotFound("payment intent", id)
	}
	return p, err
}

func requireActive(m model.Merchant, op string) error {
	if m.Status != model.MerchantActive {
		return apperr.New(apperr.CodeForbidden, "merchant account is %s; cannot %s", m.Status, op)
	}
	return nil
}

func requireOperational(m model.Merchant, op string) error {
	if m.Status != model.MerchantActive && m.Status != model.MerchantSuspended {
		return apperr.New(apperr.CodeForbidden, "merchant account is %s; cannot %s", m.Status, op)
	}
	return nil
}

// CreateInput is the request to create a payment intent.
type CreateInput struct {
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	CaptureMethod  string            `json:"capture_method"`
	Description    string            `json:"description"`
	Metadata       map[string]string `json:"metadata"`
	BillingCountry string            `json:"billing_country"`
	// Confirm authorizes immediately using PaymentMethod.
	Confirm       bool            `json:"confirm"`
	PaymentMethod *AuthorizeInput `json:"payment_method,omitempty"`
}

// Create makes a new payment intent and, with Confirm, authorizes it.
func (s *Service) Create(ctx context.Context, m model.Merchant, in CreateInput) (*model.PaymentIntent, error) {
	if err := requireActive(m, "create payments"); err != nil {
		return nil, err
	}
	var v apperr.Validator
	v.Range("amount", in.Amount, 1, s.opts.MaxAmountMinor)
	cur, err := money.NormalizeCurrency(in.Currency)
	v.Check(err == nil, "currency", "must be a supported ISO 4217 currency")
	if in.CaptureMethod == "" {
		in.CaptureMethod = string(model.CaptureManual)
	}
	v.OneOf("capture_method", in.CaptureMethod, string(model.CaptureAutomatic), string(model.CaptureManual))
	v.MaxLen("description", in.Description, 500)
	v.Metadata("metadata", in.Metadata)
	if in.BillingCountry != "" {
		v.Country("billing_country", in.BillingCountry)
	}
	v.Check(!in.Confirm || in.PaymentMethod != nil, "payment_method", "is required when confirm is true")
	v.Check(in.Confirm || in.PaymentMethod == nil, "confirm", "must be true when payment_method is given")
	if err := v.Err(); err != nil {
		return nil, err
	}
	now := s.Clock.Now()
	p := &model.PaymentIntent{
		ID: ids.New("pi"), MerchantID: m.ID, Amount: in.Amount, Currency: cur, Status: model.StatusCreated,
		CaptureMethod: model.CaptureMethod(in.CaptureMethod), Description: in.Description, Metadata: in.Metadata,
		BillingCountry: in.BillingCountry, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if p.Metadata == nil {
		p.Metadata = map[string]string{}
	}
	err = s.DB.Tx(ctx, func(q *store.Queries) error {
		if err := q.InsertPayment(ctx, p); err != nil {
			return err
		}
		if err := q.InsertHistory(ctx, model.PaymentHistory{ID: ids.New("ph"), PaymentID: p.ID, Action: "create",
			FromStatus: "", ToStatus: string(p.Status), Amount: p.Amount, CreatedAt: now}); err != nil {
			return err
		}
		if _, err := s.Outbox.Publish(ctx, q, m.ID, webhook.EventPaymentCreated, p.ID, view.PaymentIntent(p)); err != nil {
			return err
		}
		return s.Audit.Record(ctx, q, audit.Entry{Action: "payment.create", ResourceType: "payment_intent",
			ResourceID: p.ID, MerchantID: m.ID, Metadata: map[string]any{"amount": p.Amount, "currency": p.Currency,
				"capture_method": p.CaptureMethod}})
	})
	if err != nil {
		return nil, err
	}
	if s.Metrics != nil {
		s.Metrics.PaymentOps.With("create", string(p.Status)).Inc()
	}
	if in.Confirm {
		if in.PaymentMethod.BillingCountry == "" {
			in.PaymentMethod.BillingCountry = in.BillingCountry
		}
		return s.Authorize(ctx, m, p.ID, *in.PaymentMethod)
	}
	return p, nil
}

// Get returns a merchant's payment intent.
func (s *Service) Get(ctx context.Context, merchantID, id string) (*model.PaymentIntent, error) {
	return s.load(ctx, merchantID, id)
}

// List pages through payment intents.
func (s *Service) List(ctx context.Context, f store.PaymentFilter) ([]*model.PaymentIntent, bool, error) {
	if f.Status != "" {
		var v apperr.Validator
		v.OneOf("status", string(f.Status), allStatuses()...)
		if err := v.Err(); err != nil {
			return nil, false, err
		}
	}
	return s.DB.Q().ListPayments(ctx, f)
}

func allStatuses() []string {
	return []string{string(model.StatusCreated), string(model.StatusRequiresAction), string(model.StatusRequiresReview),
		string(model.StatusAuthorized), string(model.StatusCaptured), string(model.StatusPartiallyRefunded),
		string(model.StatusRefunded), string(model.StatusVoided), string(model.StatusCanceled),
		string(model.StatusDeclined), string(model.StatusBlocked)}
}

// History returns the transitions of a merchant's payment.
func (s *Service) History(ctx context.Context, merchantID, id string) ([]model.PaymentHistory, error) {
	if _, err := s.load(ctx, merchantID, id); err != nil {
		return nil, err
	}
	return s.DB.Q().ListHistory(ctx, id)
}

// Refunds lists the refunds of a merchant's payment.
func (s *Service) Refunds(ctx context.Context, merchantID, id string) ([]model.Refund, error) {
	if _, err := s.load(ctx, merchantID, id); err != nil {
		return nil, err
	}
	return s.DB.Q().ListRefunds(ctx, id)
}

// acquirerCtx bounds a simulated network call.
func (s *Service) acquirerCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.opts.AcquirerTimeout)
}

// acquirerError converts network failures into a retryable 503.
func acquirerError(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return apperr.Wrap(err, apperr.CodeUnavailable, "acquirer did not respond to %s in time; it is safe to retry", op)
	}
	return apperr.Wrap(err, apperr.CodeInternal, "acquirer rejected %s", op)
}

func normalizeReason(reason string, allowed ...string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", nil
	}
	var v apperr.Validator
	v.OneOf("reason", reason, allowed...)
	return reason, v.Err()
}
