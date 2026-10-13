package payment

import (
	"context"
	"fmt"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/audit"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
	"github.com/robertramos/enterprise-payment-gateway/internal/reqctx"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

// auditEntry is a local alias to keep call sites short.
type auditEntry = audit.Entry

// CaptureInput optionally limits the captured amount (partial capture).
type CaptureInput struct {
	Amount *int64 `json:"amount,omitempty"`
}

// Capture captures an authorization, fully or partially. Any uncaptured
// remainder is released; a payment is captured at most once.
func (s *Service) Capture(ctx context.Context, m model.Merchant, id string, in CaptureInput) (*model.PaymentIntent, error) {
	if err := requireOperational(m, "capture payments"); err != nil {
		return nil, err
	}
	unlock := s.locks.Lock(id)
	defer unlock()
	p, err := s.load(ctx, m.ID, id)
	if err != nil {
		return nil, err
	}
	if err := CheckApply(p.Status, ActCapture); err != nil {
		return nil, err
	}
	amount := p.AmountAuthorized
	if in.Amount != nil {
		amount = *in.Amount
	}
	var v apperr.Validator
	v.Range("amount", amount, 1, p.AmountAuthorized)
	if err := v.Err(); err != nil {
		return nil, err
	}
	return s.capture(ctx, p, amount)
}

// capture performs the acquirer capture; the caller holds the payment lock.
func (s *Service) capture(ctx context.Context, p *model.PaymentIntent, amount int64) (*model.PaymentIntent, error) {
	actx, cancel := s.acquirerCtx(ctx)
	ref, err := s.Acquirer.Capture(actx, p.AuthRef, amount)
	cancel()
	if err != nil {
		return nil, acquirerError("capture", err)
	}
	from := p.Status
	now := s.Clock.Now()
	p.Status = model.StatusCaptured
	p.AmountCaptured = amount
	p.CaptureRef = ref
	p.CapturedAt = &now
	p.AuthExpiresAt = nil
	detail := "captured in full"
	if amount < p.AmountAuthorized {
		detail = fmt.Sprintf("partial capture; released %s", money.FormatMinor(p.AmountAuthorized-amount, p.Currency))
	}
	err = s.commit(ctx, change{
		p: p, from: from, action: ActCapture, amount: amount, detail: detail,
		events:  []pendingEvent{paymentEvent(webhook.EventPaymentCaptured, p)},
		auditMD: map[string]any{"capture_ref": ref},
		extra: func(q *store.Queries) error {
			return s.Ledger.RecordCapture(ctx, q, ref, p.MerchantID, p.Currency, amount)
		},
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// VoidInput carries an optional cancellation reason.
type VoidInput struct {
	Reason string `json:"reason"`
}

var cancelReasons = []string{"requested_by_customer", "duplicate", "fraudulent", "abandoned"}

// Void releases an uncaptured authorization at the acquirer.
func (s *Service) Void(ctx context.Context, m model.Merchant, id string, in VoidInput) (*model.PaymentIntent, error) {
	if err := requireOperational(m, "void payments"); err != nil {
		return nil, err
	}
	reason, err := normalizeReason(in.Reason, cancelReasons...)
	if err != nil {
		return nil, err
	}
	unlock := s.locks.Lock(id)
	defer unlock()
	p, err := s.load(ctx, m.ID, id)
	if err != nil {
		return nil, err
	}
	if err := CheckApply(p.Status, ActVoid); err != nil {
		return nil, err
	}
	return s.void(ctx, p, ActVoid, reason)
}

func (s *Service) void(ctx context.Context, p *model.PaymentIntent, action Action, reason string) (*model.PaymentIntent, error) {
	actx, cancel := s.acquirerCtx(ctx)
	ref, err := s.Acquirer.Void(actx, p.AuthRef)
	cancel()
	if err != nil {
		return nil, acquirerError("void", err)
	}
	from := p.Status
	now := s.Clock.Now()
	p.Status = model.StatusVoided
	p.CanceledAt = &now
	p.CancellationReason = reason
	p.AuthExpiresAt = nil
	err = s.commit(ctx, change{p: p, from: from, action: action, amount: p.AmountAuthorized,
		detail: "authorization released", events: []pendingEvent{paymentEvent(webhook.EventPaymentVoided, p)},
		auditMD: map[string]any{"void_ref": ref, "reason": reason}})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Cancel abandons a payment that never reached authorization.
func (s *Service) Cancel(ctx context.Context, m model.Merchant, id string, in VoidInput) (*model.PaymentIntent, error) {
	if err := requireOperational(m, "cancel payments"); err != nil {
		return nil, err
	}
	reason, err := normalizeReason(in.Reason, cancelReasons...)
	if err != nil {
		return nil, err
	}
	unlock := s.locks.Lock(id)
	defer unlock()
	p, err := s.load(ctx, m.ID, id)
	if err != nil {
		return nil, err
	}
	if err := CheckApply(p.Status, ActCancel); err != nil {
		return nil, err
	}
	from := p.Status
	now := s.Clock.Now()
	p.Status = model.StatusCanceled
	p.CanceledAt = &now
	p.CancellationReason = reason
	err = s.commit(ctx, change{p: p, from: from, action: ActCancel, detail: "canceled before authorization",
		events: []pendingEvent{paymentEvent(webhook.EventPaymentCanceled, p)}, auditMD: map[string]any{"reason": reason}})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ExpireAuthorizations voids authorizations whose hold period elapsed. It
// returns the number of payments expired.
func (s *Service) ExpireAuthorizations(ctx context.Context, batch int) (int, error) {
	ctx = reqctx.WithActor(ctx, reqctx.System("authorization-expiry"))
	expired, err := s.DB.Q().ListExpiredAuthorizations(ctx, s.Clock.Now(), batch)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range expired {
		done, err := s.expireOne(ctx, id)
		if err != nil {
			s.Log.Error("authorization expiry failed", "payment_id", id, "error", err)
			continue
		}
		if done {
			n++
		}
	}
	return n, nil
}

func (s *Service) expireOne(ctx context.Context, id string) (bool, error) {
	unlock := s.locks.Lock(id)
	defer unlock()
	p, err := s.load(ctx, "", id)
	if err != nil {
		return false, err
	}
	// Re-check under the lock: it may have been captured meanwhile.
	if p.Status != model.StatusAuthorized || p.AuthExpiresAt == nil || p.AuthExpiresAt.After(s.Clock.Now()) {
		return false, nil
	}
	_, err = s.void(ctx, p, ActExpire, "authorization_expired")
	return err == nil, err
}
