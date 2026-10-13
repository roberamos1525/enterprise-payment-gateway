// Package idempotency makes mutating API requests safe to retry. A client
// sends an Idempotency-Key; the first request with that key is executed and
// its response persisted, later requests with the same key and the same
// request fingerprint receive the stored response, and requests reusing the
// key for a different payload are rejected.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

// Options configures retention.
type Options struct {
	TTL     time.Duration // how long completed responses are kept
	LockTTL time.Duration // how long an in-progress claim blocks retries
}

// Service manages idempotency records.
type Service struct {
	db    *store.DB
	clock clock.Clock
	opts  Options
}

// NewService builds the service.
func NewService(db *store.DB, clk clock.Clock, opts Options) *Service {
	if opts.TTL <= 0 {
		opts.TTL = 24 * time.Hour
	}
	if opts.LockTTL <= 0 {
		opts.LockTTL = time.Minute
	}
	return &Service{db: db, clock: clk, opts: opts}
}

// Replay is a stored response to send instead of executing the request.
type Replay struct {
	Status int
	Body   []byte
}

// Fingerprint identifies a request's semantic content.
func Fingerprint(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// ValidKey reports whether a client-supplied key is acceptable.
func ValidKey(key string) bool {
	if len(key) == 0 || len(key) > 255 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

// Begin claims key for this request. It returns a Replay when a completed
// response exists, nil when the caller should execute the request (and later
// call Finish), or an error for misuse or a concurrent in-flight request.
func (s *Service) Begin(ctx context.Context, merchantID, key, method, path string, body []byte) (*Replay, error) {
	if !ValidKey(key) {
		var v apperr.Validator
		v.Add("Idempotency-Key", "must be 1-255 printable ASCII characters")
		return nil, v.Err()
	}
	fp := Fingerprint(method, path, body)
	var replay *Replay
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		now := s.clock.Now()
		rec, err := q.GetIdempotency(ctx, merchantID, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil && rec.ExpiresAt.After(now) {
			if rec.Fingerprint != fp {
				return apperr.New(apperr.CodeIdempotencyMismatch,
					"Idempotency-Key was already used with a different request")
			}
			if rec.Completed {
				replay = &Replay{Status: rec.ResponseStatus, Body: rec.ResponseBody}
				return nil
			}
			if rec.LockedUntil.After(now) {
				return apperr.New(apperr.CodeIdempotencyInProgress,
					"a request with this Idempotency-Key is still being processed")
			}
			// The original request died without finishing; take over.
			return q.RelockIdempotency(ctx, merchantID, key, now, now.Add(s.opts.LockTTL))
		}
		return q.UpsertIdempotencyLock(ctx, model.IdempotencyRecord{
			MerchantID: merchantID, Key: key, Method: method, Path: path, Fingerprint: fp,
			CreatedAt: now, ExpiresAt: now.Add(s.opts.TTL), LockedUntil: now.Add(s.opts.LockTTL),
		})
	})
	if errors.Is(err, store.ErrConflict) {
		return nil, apperr.New(apperr.CodeIdempotencyInProgress, "a request with this Idempotency-Key is still being processed")
	}
	return replay, err
}

// Finish stores the response for future replays. Server errors (5xx) are not
// cached: the claim is released so the client can retry with the same key.
func (s *Service) Finish(ctx context.Context, merchantID, key string, status int, body []byte) error {
	if status >= 500 {
		return s.db.Q().DeleteIdempotency(ctx, merchantID, key)
	}
	return s.db.Q().CompleteIdempotency(ctx, merchantID, key, status, body)
}

// Purge deletes expired records.
func (s *Service) Purge(ctx context.Context) (int64, error) {
	return s.db.Q().PurgeIdempotency(ctx, s.clock.Now())
}
