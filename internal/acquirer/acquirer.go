// Package acquirer simulates a card acquirer / network. It never talks to a
// real network: outcomes are derived deterministically from synthetic test
// PANs, latency is simulated, and the acquirer keeps its own transaction
// books (used to produce the daily settlement file for reconciliation).
package acquirer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/card"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/metrics"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
)

// Decline codes returned by the simulated network.
const (
	DeclineDoNotHonor        = "do_not_honor"
	DeclineInsufficientFunds = "insufficient_funds"
	DeclineLostCard          = "lost_card"
	DeclineStolenCard        = "stolen_card"
	DeclineExpiredCard       = "expired_card"
	DeclineIncorrectCVC      = "incorrect_cvc"
	DeclineProcessingError   = "processing_error"
	DeclineAuthentication    = "authentication_failed"
	DeclineRefundFailed      = "refund_failed"
)

var declineMessages = map[string]string{
	DeclineDoNotHonor:        "The card was declined by the issuer.",
	DeclineInsufficientFunds: "The card has insufficient funds.",
	DeclineLostCard:          "The card was reported lost.",
	DeclineStolenCard:        "The card was reported stolen.",
	DeclineExpiredCard:       "The card has expired.",
	DeclineIncorrectCVC:      "The card's security code is incorrect.",
	DeclineProcessingError:   "The acquirer could not process the request; it is safe to retry.",
	DeclineAuthentication:    "3-D Secure authentication failed.",
	DeclineRefundFailed:      "The acquirer rejected the refund.",
}

// DeclineMessage returns the cardholder-safe message for a decline code.
func DeclineMessage(code string) string {
	if m, ok := declineMessages[code]; ok {
		return m
	}
	return "The card was declined."
}

// Errors for protocol violations (these indicate gateway bugs, not declines).
var (
	ErrUnknownRef   = errors.New("acquirer: unknown reference")
	ErrOverCapture  = errors.New("acquirer: capture exceeds authorized amount")
	ErrOverRefund   = errors.New("acquirer: refund exceeds captured amount")
	ErrNotApproved  = errors.New("acquirer: reference was not approved")
	ErrAlreadyFinal = errors.New("acquirer: authorization already voided")
)

// Config tunes the simulator.
type Config struct {
	BaseLatency     time.Duration // added to every call
	Jitter          time.Duration // uniform random extra latency
	SlowLatency     time.Duration // extra latency for the slow_network scenario
	DiscrepancyRate float64       // fraction of settlement-file rows to perturb (0 = faithful file)
	DiscrepancySeed string        // seed making perturbations deterministic
}

// Acquirer is the simulator.
type Acquirer struct {
	db      *store.DB
	vault   *vault.Service
	clock   clock.Clock
	cfg     Config
	metrics *metrics.Gateway
	sleep   func(ctx context.Context, d time.Duration) error
}

// New builds a simulator.
func New(db *store.DB, v *vault.Service, clk clock.Clock, cfg Config, m *metrics.Gateway) *Acquirer {
	return &Acquirer{db: db, vault: v, clock: clk, cfg: cfg, metrics: m, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (a *Acquirer) simulateLatency(ctx context.Context, op string, scenario card.Scenario) (func(), error) {
	d := a.cfg.BaseLatency
	if a.cfg.Jitter > 0 {
		d += rand.N(a.cfg.Jitter)
	}
	if scenario == card.ScenarioSlow {
		d += a.cfg.SlowLatency
	}
	start := time.Now()
	if err := a.sleep(ctx, d); err != nil {
		return nil, fmt.Errorf("acquirer: %s timed out: %w", op, err)
	}
	return func() {
		if a.metrics != nil {
			a.metrics.AcquirerLatency.Observe(time.Since(start).Seconds(), op)
		}
	}, nil
}

// AuthRequest asks the network to authorize an amount on a tokenized card.
type AuthRequest struct {
	MerchantID string
	PaymentID  string
	Card       model.VaultCard
	Amount     int64
	Currency   string
	// ThreeDSCompleted is true when the cardholder already passed a challenge.
	ThreeDSCompleted bool
}

// AuthResponse is the network's answer.
type AuthResponse struct {
	Approved    bool
	Requires3DS bool
	Ref         string
	AuthCode    string
	DeclineCode string
	Message     string
}

// scenarioFor reveals the PAN (in memory only) to look up its behaviour.
func (a *Acquirer) scenarioFor(vc model.VaultCard) (card.Scenario, error) {
	pan, err := a.vault.Reveal(vc)
	if err != nil {
		return "", err
	}
	if tc, ok := card.LookupTestCard(pan); ok {
		return tc.Scenario, nil
	}
	return card.ScenarioApprove, nil
}

var scenarioDeclines = map[card.Scenario]string{
	card.ScenarioDecline:           DeclineDoNotHonor,
	card.ScenarioInsufficientFunds: DeclineInsufficientFunds,
	card.ScenarioLostCard:          DeclineLostCard,
	card.ScenarioStolenCard:        DeclineStolenCard,
	card.ScenarioExpiredCard:       DeclineExpiredCard,
	card.ScenarioIncorrectCVC:      DeclineIncorrectCVC,
	card.ScenarioProcessingError:   DeclineProcessingError,
}

// Authorize places a hold on the card.
func (a *Acquirer) Authorize(ctx context.Context, req AuthRequest) (AuthResponse, error) {
	scenario, err := a.scenarioFor(req.Card)
	if err != nil {
		return AuthResponse{}, err
	}
	done, err := a.simulateLatency(ctx, "authorize", scenario)
	if err != nil {
		return AuthResponse{}, err
	}
	defer done()

	if (scenario == card.Scenario3DSApprove || scenario == card.Scenario3DSDecline) && !req.ThreeDSCompleted {
		return AuthResponse{Requires3DS: true, Message: "3-D Secure authentication required."}, nil
	}
	resp := AuthResponse{Ref: ids.New("acq")}
	if code, declined := scenarioDeclines[scenario]; declined {
		resp.DeclineCode = code
	} else if scenario == card.Scenario3DSDecline {
		resp.DeclineCode = DeclineDoNotHonor
	}
	if resp.DeclineCode != "" {
		resp.Message = DeclineMessage(resp.DeclineCode)
	} else {
		resp.Approved = true
		resp.AuthCode = authCode(resp.Ref)
		resp.Message = "Approved"
	}
	responseText := "approved:" + resp.AuthCode
	if !resp.Approved {
		responseText = "declined:" + resp.DeclineCode
	}
	err = a.db.Tx(ctx, func(q *store.Queries) error {
		return q.InsertAcquirerTxn(ctx, model.AcquirerTxn{
			Ref: resp.Ref, Kind: "authorization", MerchantID: req.MerchantID, Amount: req.Amount,
			Currency: req.Currency, Approved: resp.Approved, Response: responseText, CreatedAt: a.clock.Now(),
		})
	})
	return resp, err
}

// authCode derives a deterministic 6-character approval code from a reference.
func authCode(ref string) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	sum := sha256.Sum256([]byte(ref))
	var b strings.Builder
	for i := 0; i < 6; i++ {
		b.WriteByte(alphabet[int(sum[i])%len(alphabet)])
	}
	return b.String()
}

func (a *Acquirer) parent(ctx context.Context, q *store.Queries, ref, wantKind string) (model.AcquirerTxn, error) {
	t, err := q.GetAcquirerTxn(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return t, fmt.Errorf("%w: %s", ErrUnknownRef, ref)
	}
	if err != nil {
		return t, err
	}
	if t.Kind != wantKind {
		return t, fmt.Errorf("%w: %s is a %s, not a %s", ErrUnknownRef, ref, t.Kind, wantKind)
	}
	if !t.Approved {
		return t, fmt.Errorf("%w: %s", ErrNotApproved, ref)
	}
	return t, nil
}

// Capture settles (part of) an authorization. Only a single capture per
// authorization is accepted, mirroring the gateway's capture semantics.
func (a *Acquirer) Capture(ctx context.Context, authRef string, amount int64) (string, error) {
	done, err := a.simulateLatency(ctx, "capture", card.ScenarioApprove)
	if err != nil {
		return "", err
	}
	defer done()
	ref := ids.New("acq")
	err = a.db.Tx(ctx, func(q *store.Queries) error {
		auth, err := a.parent(ctx, q, authRef, "authorization")
		if err != nil {
			return err
		}
		if voided, err := q.SumAcquirerChildren(ctx, authRef, "void"); err != nil {
			return err
		} else if voided > 0 {
			return ErrAlreadyFinal
		}
		captured, err := q.SumAcquirerChildren(ctx, authRef, "capture")
		if err != nil {
			return err
		}
		if amount <= 0 || captured+amount > auth.Amount {
			return ErrOverCapture
		}
		return q.InsertAcquirerTxn(ctx, model.AcquirerTxn{
			Ref: ref, Kind: "capture", ParentRef: authRef, MerchantID: auth.MerchantID, Amount: amount,
			Currency: auth.Currency, Approved: true, Response: "captured", CreatedAt: a.clock.Now(),
		})
	})
	return ref, err
}

// Void releases an uncaptured authorization.
func (a *Acquirer) Void(ctx context.Context, authRef string) (string, error) {
	done, err := a.simulateLatency(ctx, "void", card.ScenarioApprove)
	if err != nil {
		return "", err
	}
	defer done()
	ref := ids.New("acq")
	err = a.db.Tx(ctx, func(q *store.Queries) error {
		auth, err := a.parent(ctx, q, authRef, "authorization")
		if err != nil {
			return err
		}
		captured, err := q.SumAcquirerChildren(ctx, authRef, "capture")
		if err != nil {
			return err
		}
		if captured > 0 {
			return fmt.Errorf("acquirer: cannot void captured authorization %s", authRef)
		}
		return q.InsertAcquirerTxn(ctx, model.AcquirerTxn{
			Ref: ref, Kind: "void", ParentRef: authRef, MerchantID: auth.MerchantID, Amount: auth.Amount,
			Currency: auth.Currency, Approved: true, Response: "voided", CreatedAt: a.clock.Now(),
		})
	})
	return ref, err
}

// RefundResponse is the outcome of a refund request.
type RefundResponse struct {
	Approved    bool
	Ref         string
	DeclineCode string
}

// Refund returns (part of) a capture to the card.
func (a *Acquirer) Refund(ctx context.Context, captureRef string, vc model.VaultCard, amount int64) (RefundResponse, error) {
	scenario, err := a.scenarioFor(vc)
	if err != nil {
		return RefundResponse{}, err
	}
	done, err := a.simulateLatency(ctx, "refund", scenario)
	if err != nil {
		return RefundResponse{}, err
	}
	defer done()
	resp := RefundResponse{Ref: ids.New("acq"), Approved: scenario != card.ScenarioRefundFails}
	if !resp.Approved {
		resp.DeclineCode = DeclineRefundFailed
	}
	err = a.db.Tx(ctx, func(q *store.Queries) error {
		capture, err := a.parent(ctx, q, captureRef, "capture")
		if err != nil {
			return err
		}
		refunded, err := q.SumAcquirerChildren(ctx, captureRef, "refund")
		if err != nil {
			return err
		}
		if amount <= 0 || refunded+amount > capture.Amount {
			return ErrOverRefund
		}
		response := "refunded"
		if !resp.Approved {
			response = "declined:" + resp.DeclineCode
		}
		return q.InsertAcquirerTxn(ctx, model.AcquirerTxn{
			Ref: resp.Ref, Kind: "refund", ParentRef: captureRef, MerchantID: capture.MerchantID, Amount: amount,
			Currency: capture.Currency, Approved: resp.Approved, Response: response, CreatedAt: a.clock.Now(),
		})
	})
	return resp, err
}
