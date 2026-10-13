// Package view renders domain objects into their public JSON shapes. It is
// shared by the HTTP API and webhook payloads so both always agree. No view
// ever contains a raw PAN: cards are shown as brand, BIN, last4 and mask.
package view

import (
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
)

// Card is the public representation of a tokenized card.
type Card struct {
	Token       string `json:"token"`
	Brand       string `json:"brand"`
	BIN         string `json:"bin"`
	Last4       string `json:"last4"`
	Masked      string `json:"masked_number"`
	Country     string `json:"country"`
	Fingerprint string `json:"fingerprint"`
	ExpMonth    int    `json:"exp_month,omitempty"`
	ExpYear     int    `json:"exp_year,omitempty"`
	Funding     string `json:"funding,omitempty"`
}

// VaultCard renders a stored card.
func VaultCard(vc model.VaultCard) Card {
	return Card{
		Token: vc.Token, Brand: vc.Brand, BIN: vc.BIN, Last4: vc.Last4, Masked: vc.BIN + "******" + vc.Last4,
		Country: vc.Country, Fingerprint: vc.Fingerprint, ExpMonth: vc.ExpMonth, ExpYear: vc.ExpYear, Funding: vc.Funding,
	}
}

// NextAction tells the client what it must do to progress a payment.
type NextAction struct {
	Type        string `json:"type"`
	ChallengeID string `json:"challenge_id,omitempty"`
	Hint        string `json:"hint,omitempty"`
}

// Payment is the public payment intent.
type Payment struct {
	ID                 string            `json:"id"`
	Object             string            `json:"object"`
	MerchantID         string            `json:"merchant_id"`
	Amount             int64             `json:"amount"`
	Currency           string            `json:"currency"`
	AmountDisplay      string            `json:"amount_display"`
	Status             string            `json:"status"`
	CaptureMethod      string            `json:"capture_method"`
	AmountAuthorized   int64             `json:"amount_authorized"`
	AmountCaptured     int64             `json:"amount_captured"`
	AmountRefunded     int64             `json:"amount_refunded"`
	AmountCapturable   int64             `json:"amount_capturable"`
	AmountRefundable   int64             `json:"amount_refundable"`
	Card               *Card             `json:"card,omitempty"`
	AuthorizationCode  string            `json:"authorization_code,omitempty"`
	AcquirerReference  string            `json:"acquirer_reference,omitempty"`
	CaptureReference   string            `json:"capture_reference,omitempty"`
	DeclineCode        string            `json:"decline_code,omitempty"`
	DeclineMessage     string            `json:"decline_message,omitempty"`
	Risk               *model.Risk       `json:"risk,omitempty"`
	NextAction         *NextAction       `json:"next_action,omitempty"`
	BillingCountry     string            `json:"billing_country,omitempty"`
	Description        string            `json:"description,omitempty"`
	Metadata           map[string]string `json:"metadata"`
	CancellationReason string            `json:"cancellation_reason,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	AuthorizedAt       *time.Time        `json:"authorized_at,omitempty"`
	CapturedAt         *time.Time        `json:"captured_at,omitempty"`
	CanceledAt         *time.Time        `json:"canceled_at,omitempty"`
	AuthExpiresAt      *time.Time        `json:"authorization_expires_at,omitempty"`
}

// PaymentIntent renders a payment intent.
func PaymentIntent(p *model.PaymentIntent) Payment {
	v := Payment{
		ID: p.ID, Object: "payment_intent", MerchantID: p.MerchantID, Amount: p.Amount, Currency: p.Currency,
		AmountDisplay: money.FormatMinor(p.Amount, p.Currency) + " " + p.Currency,
		Status:        string(p.Status), CaptureMethod: string(p.CaptureMethod),
		AmountAuthorized: p.AmountAuthorized, AmountCaptured: p.AmountCaptured, AmountRefunded: p.AmountRefunded,
		AmountRefundable: p.Refundable(), AuthorizationCode: p.AuthCode, AcquirerReference: p.AuthRef,
		CaptureReference: p.CaptureRef, DeclineCode: p.DeclineCode, DeclineMessage: p.DeclineMessage,
		BillingCountry: p.BillingCountry, Description: p.Description, Metadata: p.Metadata,
		CancellationReason: p.CancellationReason, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		AuthorizedAt: p.AuthorizedAt, CapturedAt: p.CapturedAt, CanceledAt: p.CanceledAt, AuthExpiresAt: p.AuthExpiresAt,
	}
	if v.Metadata == nil {
		v.Metadata = map[string]string{}
	}
	if p.Status == model.StatusAuthorized {
		v.AmountCapturable = p.AmountAuthorized
	}
	if p.Card != nil {
		v.Card = &Card{Token: p.Card.Token, Brand: p.Card.Brand, BIN: p.Card.BIN, Last4: p.Card.Last4,
			Masked: p.Card.Masked(), Country: p.Card.Country, Fingerprint: p.Card.Fingerprint}
	}
	if p.Risk.Action != "" {
		r := p.Risk
		if r.Reasons == nil {
			r.Reasons = []string{}
		}
		v.Risk = &r
	}
	switch p.Status {
	case model.StatusRequiresAction:
		v.NextAction = &NextAction{Type: "three_d_secure", ChallengeID: p.ChallengeID,
			Hint: "POST /v1/payment_intents/{id}/3ds with challenge_id and outcome=success|failure"}
	case model.StatusRequiresReview:
		v.NextAction = &NextAction{Type: "manual_review", Hint: "awaiting an operator review decision"}
	}
	return v
}

// PaymentList renders a slice of payment intents.
func PaymentList(ps []*model.PaymentIntent) []Payment {
	out := make([]Payment, len(ps))
	for i, p := range ps {
		out[i] = PaymentIntent(p)
	}
	return out
}

// Refund is the public refund.
type Refund struct {
	model.Refund
	Object        string `json:"object"`
	AmountDisplay string `json:"amount_display"`
}

// RefundOf renders a refund.
func RefundOf(r model.Refund) Refund {
	return Refund{Refund: r, Object: "refund", AmountDisplay: money.FormatMinor(r.Amount, r.Currency) + " " + r.Currency}
}

// List is the envelope for paginated collections.
type List[T any] struct {
	Object     string `json:"object"`
	Data       []T    `json:"data"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// NewList builds a list envelope; cursor is the id of the last element when more exist.
func NewList[T any](items []T, hasMore bool, lastID func(T) string) List[T] {
	if items == nil {
		items = []T{}
	}
	l := List[T]{Object: "list", Data: items, HasMore: hasMore}
	if hasMore && len(items) > 0 {
		l.NextCursor = lastID(items[len(items)-1])
	}
	return l
}
