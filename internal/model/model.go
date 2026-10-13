// Package model holds the gateway's persistent entities. It contains plain
// data types only; behaviour lives in the service packages.
package model

import (
	"encoding/json"
	"time"
)

// MerchantStatus is the lifecycle state of a merchant account.
type MerchantStatus string

// Merchant statuses.
const (
	MerchantPending   MerchantStatus = "pending"
	MerchantActive    MerchantStatus = "active"
	MerchantSuspended MerchantStatus = "suspended"
	MerchantClosed    MerchantStatus = "closed"
)

// Merchant is an onboarded business that accepts payments.
type Merchant struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Email           string         `json:"email"`
	Country         string         `json:"country"`
	DefaultCurrency string         `json:"default_currency"`
	Status          MerchantStatus `json:"status"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// FeeSchedule is a versioned pricing plan. The schedule with the latest
// EffectiveFrom not after a settlement day applies to that day.
type FeeSchedule struct {
	ID                string    `json:"id"`
	MerchantID        string    `json:"merchant_id"`
	PercentBps        int64     `json:"percent_bps"`
	FixedMinor        int64     `json:"fixed_minor"`
	RefundFixedMinor  int64     `json:"refund_fixed_minor"`
	ReturnFeeOnRefund bool      `json:"return_fee_on_refund"`
	EffectiveFrom     time.Time `json:"effective_from"`
	CreatedAt         time.Time `json:"created_at"`
}

// APIKeyStatus is the state of an API key.
type APIKeyStatus string

// API key statuses.
const (
	APIKeyActive  APIKeyStatus = "active"
	APIKeyRevoked APIKeyStatus = "revoked"
)

// APIKey is a merchant credential. Only a hash of the secret is stored; the
// prefix identifies the key without revealing the secret.
type APIKey struct {
	ID          string       `json:"id"`
	MerchantID  string       `json:"merchant_id"`
	Prefix      string       `json:"prefix"`
	Hash        string       `json:"-"`
	Label       string       `json:"label"`
	Status      APIKeyStatus `json:"status"`
	RotatedFrom string       `json:"rotated_from,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	ExpiresAt   *time.Time   `json:"expires_at,omitempty"`
	RevokedAt   *time.Time   `json:"revoked_at,omitempty"`
	LastUsedAt  *time.Time   `json:"last_used_at,omitempty"`
}

// Usable reports whether the key authenticates at time now.
func (k APIKey) Usable(now time.Time) bool {
	if k.Status != APIKeyActive {
		return false
	}
	return k.ExpiresAt == nil || now.Before(*k.ExpiresAt)
}

// EndpointStatus is the state of a webhook endpoint.
type EndpointStatus string

// Endpoint statuses.
const (
	EndpointEnabled  EndpointStatus = "enabled"
	EndpointDisabled EndpointStatus = "disabled"
)

// WebhookEndpoint is a merchant URL that receives signed event notifications.
// Signing secrets are stored sealed (AES-GCM) because they must be recovered
// to compute signatures.
type WebhookEndpoint struct {
	ID                  string         `json:"id"`
	MerchantID          string         `json:"merchant_id"`
	URL                 string         `json:"url"`
	Description         string         `json:"description"`
	Events              []string       `json:"events"`
	Status              EndpointStatus `json:"status"`
	SecretSealed        string         `json:"-"`
	PrevSecretSealed    string         `json:"-"`
	PrevSecretExpiresAt *time.Time     `json:"previous_secret_expires_at,omitempty"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
}

// Subscribes reports whether the endpoint wants events of the given type.
func (e WebhookEndpoint) Subscribes(eventType string) bool {
	for _, t := range e.Events {
		if t == "*" || t == eventType {
			return true
		}
	}
	return false
}

// VaultCard is a tokenized card. Ciphertext holds the AES-GCM sealed PAN;
// everything else is non-sensitive display or routing data.
type VaultCard struct {
	Token       string
	MerchantID  string
	Fingerprint string
	KeyID       string
	Ciphertext  []byte
	Brand       string
	BIN         string
	Last4       string
	ExpMonth    int
	ExpYear     int
	Country     string
	Funding     string
	CreatedAt   time.Time
}

// Event is an immutable domain event; it doubles as the webhook outbox record.
type Event struct {
	ID         string          `json:"id"`
	MerchantID string          `json:"merchant_id"`
	Type       string          `json:"type"`
	ResourceID string          `json:"resource_id"`
	Payload    json.RawMessage `json:"data"`
	CreatedAt  time.Time       `json:"created_at"`
}

// DeliveryStatus is the state of a webhook delivery.
type DeliveryStatus string

// Delivery statuses.
const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliverySucceeded DeliveryStatus = "succeeded"
	DeliveryFailed    DeliveryStatus = "failed"
)

// Delivery tracks sending one event to one endpoint, across retries.
type Delivery struct {
	ID             string         `json:"id"`
	EventID        string         `json:"event_id"`
	EndpointID     string         `json:"endpoint_id"`
	MerchantID     string         `json:"merchant_id"`
	Status         DeliveryStatus `json:"status"`
	Attempts       int            `json:"attempts"`
	NextAttemptAt  time.Time      `json:"next_attempt_at"`
	LockedUntil    time.Time      `json:"-"`
	LastStatusCode int            `json:"last_status_code"`
	LastError      string         `json:"last_error,omitempty"`
	ReplayOf       string         `json:"replay_of,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	CompletedAt    *time.Time     `json:"completed_at,omitempty"`
}

// DeliveryAttempt is one HTTP attempt of a delivery (the delivery log).
type DeliveryAttempt struct {
	ID              string    `json:"id"`
	DeliveryID      string    `json:"delivery_id"`
	Attempt         int       `json:"attempt"`
	StatusCode      int       `json:"status_code"`
	DurationMS      int64     `json:"duration_ms"`
	Error           string    `json:"error,omitempty"`
	ResponseSnippet string    `json:"response_snippet,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// AuditEntry is one record of the append-only, hash-chained audit log.
type AuditEntry struct {
	Seq          int64           `json:"seq"`
	ID           string          `json:"id"`
	CreatedAt    time.Time       `json:"created_at"`
	ActorType    string          `json:"actor_type"`
	ActorID      string          `json:"actor_id"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	MerchantID   string          `json:"merchant_id,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	IP           string          `json:"ip,omitempty"`
	Metadata     json.RawMessage `json:"metadata"`
	PrevHash     string          `json:"prev_hash"`
	Hash         string          `json:"hash"`
}

// BlockedBIN is a fraud blocklist entry.
type BlockedBIN struct {
	BIN       string    `json:"bin"`
	Reason    string    `json:"reason"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// IdempotencyRecord persists the outcome of an idempotent request.
type IdempotencyRecord struct {
	MerchantID     string
	Key            string
	Method         string
	Path           string
	Fingerprint    string
	Completed      bool
	ResponseStatus int
	ResponseBody   []byte
	CreatedAt      time.Time
	ExpiresAt      time.Time
	LockedUntil    time.Time
}
