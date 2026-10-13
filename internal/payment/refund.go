package payment

import (
	"context"

	"github.com/robertramos/enterprise-payment-gateway/internal/acquirer"
	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/view"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

// RefundInput requests a full (amount omitted) or partial refund.
type RefundInput struct {
	Amount *int64 `json:"amount,omitempty"`
	Reason string `json:"reason"`
}

var refundReasons = []string{"requested_by_customer", "duplicate", "fraudulent"}

// Refund returns captured funds. Multiple partial refunds are allowed until
// the captured amount is exhausted. A refund rejected by the acquirer is
// persisted with status "failed" and leaves the payment unchanged.
func (s *Service) Refund(ctx context.Context, m model.Merchant, id string, in RefundInput) (model.Refund, *model.PaymentIntent, error) {
	if err := requireOperational(m, "refund payments"); err != nil {
		return model.Refund{}, nil, err
	}
	reason, err := normalizeReason(in.Reason, refundReasons...)
	if err != nil {
		return model.Refund{}, nil, err
	}
	unlock := s.locks.Lock(id)
	defer unlock()
	p, err := s.load(ctx, m.ID, id)
	if err != nil {
		return model.Refund{}, nil, err
	}
	if err := CheckApply(p.Status, ActRefund); err != nil {
		return model.Refund{}, nil, err
	}
	amount := p.Refundable()
	if in.Amount != nil {
		amount = *in.Amount
	}
	var v apperr.Validator
	if !v.Range("amount", amount, 1, p.Refundable()) {
		return model.Refund{}, nil, v.Err()
	}
	vc, err := s.Vault.Lookup(ctx, s.DB.Q(), p.MerchantID, p.Card.Token)
	if err != nil {
		return model.Refund{}, nil, err
	}
	actx, cancel := s.acquirerCtx(ctx)
	resp, err := s.Acquirer.Refund(actx, p.CaptureRef, vc, amount)
	cancel()
	if err != nil {
		return model.Refund{}, nil, acquirerError("refund", err)
	}
	r := model.Refund{
		ID: ids.New("re"), PaymentID: p.ID, MerchantID: p.MerchantID, Amount: amount, Currency: p.Currency,
		Reason: reason, AcquirerRef: resp.Ref, CreatedAt: s.Clock.Now(),
	}
	if !resp.Approved {
		r.Status, r.FailureCode = model.RefundFailed, resp.DeclineCode
		err := s.DB.Tx(ctx, func(q *store.Queries) error {
			if err := q.InsertRefund(ctx, r); err != nil {
				return err
			}
			if _, err := s.Outbox.Publish(ctx, q, p.MerchantID, webhook.EventRefundFailed, r.ID, view.RefundOf(r)); err != nil {
				return err
			}
			return s.Audit.Record(ctx, q, auditEntry{Action: "payment.refund_failed", ResourceType: "refund",
				ResourceID: r.ID, MerchantID: p.MerchantID, Metadata: map[string]any{"payment_intent": p.ID,
					"amount": amount, "failure_code": r.FailureCode, "message": acquirer.DeclineMessage(r.FailureCode)}})
		})
		if s.Metrics != nil && err == nil {
			s.Metrics.PaymentOps.With("refund", "failed").Inc()
		}
		return r, p, err
	}
	r.Status = model.RefundSucceeded
	from := p.Status
	p.AmountRefunded += amount
	if p.AmountRefunded == p.AmountCaptured {
		p.Status = model.StatusRefunded
	} else {
		p.Status = model.StatusPartiallyRefunded
	}
	err = s.commit(ctx, change{
		p: p, from: from, action: ActRefund, amount: amount, detail: "refund " + r.ID,
		events:  []pendingEvent{{typ: webhook.EventRefundSucceeded, resourceID: r.ID, data: view.RefundOf(r)}},
		auditMD: map[string]any{"refund_id": r.ID, "acquirer_ref": r.AcquirerRef},
		extra: func(q *store.Queries) error {
			if err := q.InsertRefund(ctx, r); err != nil {
				return err
			}
			return s.Ledger.RecordRefund(ctx, q, r.AcquirerRef, p.MerchantID, p.Currency, amount)
		},
	})
	if err != nil {
		return model.Refund{}, nil, err
	}
	return r, p, nil
}

// GetRefund loads one of a merchant's refunds.
func (s *Service) GetRefund(ctx context.Context, merchantID, id string) (model.Refund, error) {
	r, err := s.DB.Q().GetRefund(ctx, id)
	if err != nil || r.MerchantID != merchantID {
		if err == nil || err == store.ErrNotFound {
			return model.Refund{}, apperr.NotFound("refund", id)
		}
		return model.Refund{}, err
	}
	return r, nil
}
