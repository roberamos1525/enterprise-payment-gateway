package model

import "time"

// PaymentStatus is a state of the payment-intent state machine.
type PaymentStatus string

// Payment statuses. See internal/payment/statemachine.go for transitions.
const (
	StatusCreated           PaymentStatus = "created"
	StatusRequiresAction    PaymentStatus = "requires_action"
	StatusRequiresReview    PaymentStatus = "requires_review"
	StatusAuthorized        PaymentStatus = "authorized"
	StatusCaptured          PaymentStatus = "captured"
	StatusPartiallyRefunded PaymentStatus = "partially_refunded"
	StatusRefunded          PaymentStatus = "refunded"
	StatusVoided            PaymentStatus = "voided"
	StatusCanceled          PaymentStatus = "canceled"
	StatusDeclined          PaymentStatus = "declined"
	StatusBlocked           PaymentStatus = "blocked"
)

// CaptureMethod controls whether authorization captures immediately.
type CaptureMethod string

// Capture methods.
const (
	CaptureAutomatic CaptureMethod = "automatic"
	CaptureManual    CaptureMethod = "manual"
)

// CardSummary is the non-sensitive card data attached to a payment.
type CardSummary struct {
	Token       string `json:"token"`
	Brand       string `json:"brand"`
	BIN         string `json:"bin"`
	Last4       string `json:"last4"`
	Country     string `json:"country"`
	Fingerprint string `json:"fingerprint"`
}

// Masked returns the display form of the card (first six, last four).
func (c CardSummary) Masked() string {
	if c.BIN == "" {
		return ""
	}
	return c.BIN + "******" + c.Last4
}

// Risk is the fraud engine's verdict stored on a payment.
type Risk struct {
	Score   int      `json:"score"`
	Action  string   `json:"action"`
	Reasons []string `json:"reasons"`
}

// PaymentIntent is the aggregate root of the payment lifecycle.
type PaymentIntent struct {
	ID                 string
	MerchantID         string
	Amount             int64
	Currency           string
	Status             PaymentStatus
	CaptureMethod      CaptureMethod
	AmountAuthorized   int64
	AmountCaptured     int64
	AmountRefunded     int64
	Card               *CardSummary
	AuthCode           string
	AuthRef            string
	CaptureRef         string
	DeclineCode        string
	DeclineMessage     string
	Risk               Risk
	BillingCountry     string
	CustomerIP         string
	ChallengeID        string
	Description        string
	Metadata           map[string]string
	CancellationReason string
	Version            int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
	AuthorizedAt       *time.Time
	CapturedAt         *time.Time
	CanceledAt         *time.Time
	AuthExpiresAt      *time.Time
}

// Refundable returns the captured amount not yet refunded.
func (p *PaymentIntent) Refundable() int64 { return p.AmountCaptured - p.AmountRefunded }

// PaymentHistory is one recorded state transition.
type PaymentHistory struct {
	ID         string    `json:"id"`
	PaymentID  string    `json:"payment_id"`
	Action     string    `json:"action"`
	FromStatus string    `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	Amount     int64     `json:"amount"`
	Detail     string    `json:"detail,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// RefundStatus is the outcome of a refund.
type RefundStatus string

// Refund statuses.
const (
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

// Refund returns captured funds to the cardholder (possibly partially).
type Refund struct {
	ID          string       `json:"id"`
	PaymentID   string       `json:"payment_intent_id"`
	MerchantID  string       `json:"merchant_id"`
	Amount      int64        `json:"amount"`
	Currency    string       `json:"currency"`
	Status      RefundStatus `json:"status"`
	Reason      string       `json:"reason,omitempty"`
	AcquirerRef string       `json:"acquirer_reference,omitempty"`
	FailureCode string       `json:"failure_code,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
}
