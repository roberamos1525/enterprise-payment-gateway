package model

import "time"

// SettlementBatch aggregates one merchant's captures and refunds for one
// UTC day and currency.
type SettlementBatch struct {
	ID            string    `json:"id"`
	MerchantID    string    `json:"merchant_id"`
	Date          string    `json:"settlement_date"`
	Currency      string    `json:"currency"`
	GrossCaptured int64     `json:"gross_captured"`
	Refunded      int64     `json:"refunded"`
	Fees          int64     `json:"fees"`
	Net           int64     `json:"net"`
	CaptureCount  int       `json:"capture_count"`
	RefundCount   int       `json:"refund_count"`
	FeeScheduleID string    `json:"fee_schedule_id"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	PayoutID      string    `json:"payout_id,omitempty"`
}

// Settlement item kinds.
const (
	ItemCapture = "capture"
	ItemRefund  = "refund"
)

// SettlementItem is one capture or refund included in a batch.
type SettlementItem struct {
	BatchID    string    `json:"batch_id"`
	Kind       string    `json:"kind"`
	RefID      string    `json:"ref_id"`
	PaymentID  string    `json:"payment_intent_id"`
	Amount     int64     `json:"amount"`
	Fee        int64     `json:"fee"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Payout is money owed to a merchant from a settlement batch.
type Payout struct {
	ID           string     `json:"id"`
	BatchID      string     `json:"batch_id"`
	MerchantID   string     `json:"merchant_id"`
	Amount       int64      `json:"amount"`
	Currency     string     `json:"currency"`
	Status       string     `json:"status"`
	ScheduledFor string     `json:"scheduled_for"`
	CreatedAt    time.Time  `json:"created_at"`
	PaidAt       *time.Time `json:"paid_at,omitempty"`
}

// SettleableCapture is a captured payment eligible for settlement.
type SettleableCapture struct {
	PaymentID  string
	MerchantID string
	CaptureRef string
	Amount     int64
	Currency   string
	CapturedAt time.Time
}

// SettleableRefund is a succeeded refund eligible for settlement.
type SettleableRefund struct {
	RefundID    string
	PaymentID   string
	MerchantID  string
	AcquirerRef string
	Amount      int64
	Currency    string
	CreatedAt   time.Time
}

// AcquirerTxn is a record kept by the simulated acquirer, independent of the
// gateway's own books. Reconciliation compares the two.
type AcquirerTxn struct {
	Ref        string    `json:"ref"`
	Kind       string    `json:"kind"`
	ParentRef  string    `json:"parent_ref,omitempty"`
	MerchantID string    `json:"merchant_id"`
	Amount     int64     `json:"amount"`
	Currency   string    `json:"currency"`
	Approved   bool      `json:"approved"`
	Response   string    `json:"response"`
	CreatedAt  time.Time `json:"created_at"`
}

// ReconciliationRun summarises one comparison of internal records against an
// acquirer settlement file.
type ReconciliationRun struct {
	ID               string           `json:"id"`
	Date             string           `json:"settlement_date"`
	Source           string           `json:"source"`
	InternalCount    int              `json:"internal_count"`
	AcquirerCount    int              `json:"acquirer_count"`
	MatchedCount     int              `json:"matched_count"`
	DiscrepancyCount int              `json:"discrepancy_count"`
	Status           string           `json:"status"`
	CreatedAt        time.Time        `json:"created_at"`
	Discrepancies    []Discrepancy    `json:"discrepancies,omitempty"`
	Totals           []CurrencyTotals `json:"totals,omitempty"`
}

// CurrencyTotals compares internal and acquirer net totals for one currency.
type CurrencyTotals struct {
	Currency string `json:"currency"`
	Internal int64  `json:"internal_net"`
	Acquirer int64  `json:"acquirer_net"`
	Diff     int64  `json:"difference"`
}

// Discrepancy is one mismatch found by reconciliation.
type Discrepancy struct {
	ID             string `json:"id"`
	RunID          string `json:"run_id"`
	Kind           string `json:"kind"`
	AcquirerRef    string `json:"acquirer_ref"`
	MerchantID     string `json:"merchant_id"`
	InternalAmount *int64 `json:"internal_amount,omitempty"`
	AcquirerAmount *int64 `json:"acquirer_amount,omitempty"`
	Currency       string `json:"currency"`
	Detail         string `json:"detail"`
}

// JournalEntry is one line of a double-entry journal.
type JournalEntry struct {
	Account string `json:"account"`
	Debit   int64  `json:"debit"`
	Credit  int64  `json:"credit"`
}

// Journal is a balanced set of ledger entries for one business event.
type Journal struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Ref        string         `json:"ref"`
	MerchantID string         `json:"merchant_id"`
	Currency   string         `json:"currency"`
	CreatedAt  time.Time      `json:"created_at"`
	Entries    []JournalEntry `json:"entries"`
}

// AccountBalance is the aggregate of all entries posted to an account.
type AccountBalance struct {
	Account    string `json:"account"`
	MerchantID string `json:"merchant_id"`
	Currency   string `json:"currency"`
	Debits     int64  `json:"debits"`
	Credits    int64  `json:"credits"`
}
