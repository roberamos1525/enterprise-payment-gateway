package payment

import (
	"context"
	"strings"

	"github.com/robertramos/enterprise-payment-gateway/internal/acquirer"
	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/fraud"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/reqctx"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

// AuthorizeInput supplies the payment method for authorization: either raw
// card data (tokenized immediately) or an existing token.
type AuthorizeInput struct {
	Card           *card.Input `json:"card,omitempty"`
	Token          string      `json:"token,omitempty"`
	BillingCountry string      `json:"billing_country,omitempty"`
}

// Tokenize validates and vaults a card for the merchant.
func (s *Service) Tokenize(ctx context.Context, m model.Merchant, in card.Input) (model.VaultCard, error) {
	if err := requireActive(m, "tokenize cards"); err != nil {
		return model.VaultCard{}, err
	}
	vc, err := card.Validate(in, s.Clock.Now())
	if err != nil {
		return model.VaultCard{}, err
	}
	var out model.VaultCard
	err = s.DB.Tx(ctx, func(q *store.Queries) error {
		var err error
		if out, err = s.Vault.Tokenize(ctx, q, m.ID, vc); err != nil {
			return err
		}
		return s.Audit.Record(ctx, q, auditCardEntry(out))
	})
	return out, err
}

func auditCardEntry(vc model.VaultCard) auditEntry {
	return auditEntry{Action: "card.tokenize", ResourceType: "card_token", ResourceID: vc.Token,
		MerchantID: vc.MerchantID, Metadata: map[string]any{"brand": vc.Brand, "last4": vc.Last4, "key_id": vc.KeyID}}
}

// resolveCard returns the vault card for an authorization request,
// tokenizing raw card data when necessary.
func (s *Service) resolveCard(ctx context.Context, m model.Merchant, in AuthorizeInput) (model.VaultCard, error) {
	var v apperr.Validator
	v.Check((in.Card == nil) != (in.Token == ""), "payment_method", "provide exactly one of card or token")
	if in.BillingCountry != "" {
		v.Country("billing_country", in.BillingCountry)
	}
	if err := v.Err(); err != nil {
		return model.VaultCard{}, err
	}
	if in.Card != nil {
		return s.Tokenize(ctx, m, *in.Card)
	}
	vc, err := s.Vault.Lookup(ctx, s.DB.Q(), m.ID, strings.TrimSpace(in.Token))
	if err != nil {
		return model.VaultCard{}, err
	}
	if card.Expired(vc.ExpMonth, vc.ExpYear, s.Clock.Now()) {
		return model.VaultCard{}, apperr.Declined(acquirer.DeclineExpiredCard, acquirer.DeclineMessage(acquirer.DeclineExpiredCard))
	}
	return vc, nil
}

// Authorize screens and authorizes a payment intent.
func (s *Service) Authorize(ctx context.Context, m model.Merchant, id string, in AuthorizeInput) (*model.PaymentIntent, error) {
	if err := requireActive(m, "authorize payments"); err != nil {
		return nil, err
	}
	unlock := s.locks.Lock(id)
	defer unlock()
	p, err := s.load(ctx, m.ID, id)
	if err != nil {
		return nil, err
	}
	if err := CheckApply(p.Status, ActAuthorize); err != nil {
		return nil, err
	}
	vc, err := s.resolveCard(ctx, m, in)
	if err != nil {
		return nil, err
	}
	if in.BillingCountry != "" {
		p.BillingCountry = in.BillingCountry
	}
	if ip := reqctx.ClientIP(ctx); ip != "" {
		p.CustomerIP = ip
	}
	p.Card = &model.CardSummary{Token: vc.Token, Brand: vc.Brand, BIN: vc.BIN, Last4: vc.Last4,
		Country: vc.Country, Fingerprint: vc.Fingerprint}
	p.DeclineCode, p.DeclineMessage = "", ""

	decision, err := s.Fraud.Evaluate(ctx, fraud.Input{
		MerchantID: m.ID, CardFingerprint: vc.Fingerprint, BIN: vc.BIN, CardCountry: vc.Country,
		BillingCountry: p.BillingCountry, IP: p.CustomerIP, Amount: p.Amount, Currency: p.Currency,
	}, s.Clock.Now())
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if s.Metrics != nil {
		s.Metrics.FraudDecisions.With(string(decision.Action)).Inc()
	}
	p.Risk = model.Risk{Score: decision.Score, Action: string(decision.Action), Reasons: decision.Reasons()}
	from := p.Status
	riskMD := map[string]any{"risk_score": decision.Score, "risk_action": decision.Action}
	switch decision.Action {
	case fraud.Block:
		p.Status = model.StatusBlocked
		p.DeclineCode, p.DeclineMessage = "fraud_blocked", "The payment was blocked by risk controls."
		err := s.commit(ctx, change{p: p, from: from, action: ActAuthorize, amount: p.Amount, detail: "blocked by fraud engine",
			events: []pendingEvent{paymentEvent(webhook.EventPaymentBlocked, p)}, auditMD: riskMD})
		return p, err
	case fraud.Review:
		p.Status = model.StatusRequiresReview
		err := s.commit(ctx, change{p: p, from: from, action: ActAuthorize, amount: p.Amount, detail: "held for manual review",
			events: []pendingEvent{paymentEvent(webhook.EventPaymentRequiresReview, p)}, auditMD: riskMD})
		return p, err
	}
	return s.sendToAcquirer(ctx, p, vc, false, ActAuthorize)
}

// sendToAcquirer performs the network authorization and records the outcome.
// The caller holds the payment lock.
func (s *Service) sendToAcquirer(ctx context.Context, p *model.PaymentIntent, vc model.VaultCard, threeDSDone bool, action Action) (*model.PaymentIntent, error) {
	actx, cancel := s.acquirerCtx(ctx)
	resp, err := s.Acquirer.Authorize(actx, acquirer.AuthRequest{
		MerchantID: p.MerchantID, PaymentID: p.ID, Card: vc, Amount: p.Amount, Currency: p.Currency,
		ThreeDSCompleted: threeDSDone,
	})
	cancel()
	if err != nil {
		return nil, acquirerError("authorization", err)
	}
	from := p.Status
	now := s.Clock.Now()
	c := change{p: p, from: from, action: action, amount: p.Amount}
	switch {
	case resp.Requires3DS:
		p.Status = model.StatusRequiresAction
		p.ChallengeID = ids.New("3ds")
		c.detail = "3-D Secure challenge issued"
		c.events = []pendingEvent{paymentEvent(webhook.EventPaymentRequiresAction, p)}
	case resp.Approved:
		p.Status = model.StatusAuthorized
		p.AmountAuthorized = p.Amount
		p.AuthCode, p.AuthRef = resp.AuthCode, resp.Ref
		p.AuthorizedAt = &now
		exp := now.Add(s.opts.AuthorizationTTL)
		p.AuthExpiresAt = &exp
		c.detail = "approved " + resp.AuthCode
		c.events = []pendingEvent{paymentEvent(webhook.EventPaymentAuthorized, p)}
	default:
		p.Status = model.StatusDeclined
		p.AuthRef = resp.Ref
		p.DeclineCode, p.DeclineMessage = resp.DeclineCode, resp.Message
		c.detail = "declined " + resp.DeclineCode
		c.events = []pendingEvent{paymentEvent(webhook.EventPaymentDeclined, p)}
	}
	c.auditMD = map[string]any{"acquirer_ref": resp.Ref, "decline_code": resp.DeclineCode}
	if err := s.commit(ctx, c); err != nil {
		return nil, err
	}
	if p.Status == model.StatusAuthorized && p.CaptureMethod == model.CaptureAutomatic {
		return s.capture(ctx, p, p.AmountAuthorized)
	}
	return p, nil
}

// ChallengeInput completes a simulated 3-D Secure challenge.
type ChallengeInput struct {
	ChallengeID string `json:"challenge_id"`
	Outcome     string `json:"outcome"` // success | failure
}

// Complete3DS resolves a pending 3-D Secure challenge.
func (s *Service) Complete3DS(ctx context.Context, m model.Merchant, id string, in ChallengeInput) (*model.PaymentIntent, error) {
	if err := requireActive(m, "authorize payments"); err != nil {
		return nil, err
	}
	var v apperr.Validator
	v.Required("challenge_id", in.ChallengeID)
	v.OneOf("outcome", in.Outcome, "success", "failure")
	if err := v.Err(); err != nil {
		return nil, err
	}
	unlock := s.locks.Lock(id)
	defer unlock()
	p, err := s.load(ctx, m.ID, id)
	if err != nil {
		return nil, err
	}
	if err := CheckApply(p.Status, ActComplete3DS); err != nil {
		return nil, err
	}
	if in.ChallengeID != p.ChallengeID {
		return nil, apperr.New(apperr.CodeInvalidRequest, "challenge_id does not match the pending challenge")
	}
	if in.Outcome == "failure" {
		from := p.Status
		p.Status = model.StatusDeclined
		p.DeclineCode = acquirer.DeclineAuthentication
		p.DeclineMessage = acquirer.DeclineMessage(acquirer.DeclineAuthentication)
		err := s.commit(ctx, change{p: p, from: from, action: ActComplete3DS, amount: p.Amount,
			detail: "cardholder failed 3-D Secure", events: []pendingEvent{paymentEvent(webhook.EventPaymentDeclined, p)}})
		return p, err
	}
	vc, err := s.Vault.Lookup(ctx, s.DB.Q(), p.MerchantID, p.Card.Token)
	if err != nil {
		return nil, err
	}
	return s.sendToAcquirer(ctx, p, vc, true, ActComplete3DS)
}

// ReviewInput is an operator's decision on a payment held for review.
type ReviewInput struct {
	Decision string `json:"decision"` // approve | reject
	Note     string `json:"note"`
}

// Review applies a manual review decision (admin only).
func (s *Service) Review(ctx context.Context, id string, in ReviewInput) (*model.PaymentIntent, error) {
	var v apperr.Validator
	v.OneOf("decision", in.Decision, "approve", "reject")
	v.MaxLen("note", in.Note, 500)
	if err := v.Err(); err != nil {
		return nil, err
	}
	unlock := s.locks.Lock(id)
	defer unlock()
	p, err := s.load(ctx, "", id)
	if err != nil {
		return nil, err
	}
	action := ActReviewApprove
	if in.Decision == "reject" {
		action = ActReviewReject
	}
	if err := CheckApply(p.Status, action); err != nil {
		return nil, err
	}
	if action == ActReviewReject {
		from := p.Status
		p.Status = model.StatusBlocked
		p.DeclineCode, p.DeclineMessage = "fraud_review_rejected", "The payment was rejected after manual review."
		err := s.commit(ctx, change{p: p, from: from, action: action, amount: p.Amount, detail: in.Note,
			events: []pendingEvent{paymentEvent(webhook.EventPaymentBlocked, p)}, auditMD: map[string]any{"note": in.Note}})
		return p, err
	}
	vc, err := s.Vault.Lookup(ctx, s.DB.Q(), p.MerchantID, p.Card.Token)
	if err != nil {
		return nil, err
	}
	return s.sendToAcquirer(ctx, p, vc, false, ActReviewApprove)
}
