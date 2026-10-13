package merchant

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/audit"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
)

// API keys look like "sk_test_<8 char id>_<32 char secret>". The part before
// the last underscore is the public prefix used for lookup; only a SHA-256
// hash of the full key is stored. Keys carry 160 bits of entropy, so a fast
// hash is appropriate (no password-style stretching is needed).
const keyScheme = "sk_test_"

// IssuedKey is returned exactly once when a key is created; Secret is the
// full key and is never retrievable again.
type IssuedKey struct {
	model.APIKey
	Secret string `json:"secret"`
}

// HashKey returns the stored hash of a full API key.
func HashKey(full string) string {
	sum := sha256.Sum256([]byte(full))
	return hex.EncodeToString(sum[:])
}

// ParseKey splits a full key into its prefix and validates the format.
func ParseKey(full string) (prefix string, ok bool) {
	rest, found := strings.CutPrefix(full, keyScheme)
	if !found {
		return "", false
	}
	id, secret, found := strings.Cut(rest, "_")
	if !found || len(id) != 8 || len(secret) != 32 {
		return "", false
	}
	return keyScheme + id, true
}

func newKeyMaterial() (prefix, full string) {
	prefix = keyScheme + ids.RandomToken(5)   // 8 chars
	full = prefix + "_" + ids.RandomToken(20) // 32 chars
	return prefix, full
}

// CreateKeyInput describes a new key.
type CreateKeyInput struct {
	Label     string     `json:"label"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// CreateKey issues a new API key for an existing, non-closed merchant.
func (s *Service) CreateKey(ctx context.Context, merchantID string, in CreateKeyInput) (IssuedKey, error) {
	var v apperr.Validator
	v.MaxLen("label", in.Label, 100)
	now := s.clock.Now()
	if in.ExpiresAt != nil {
		v.Check(in.ExpiresAt.After(now), "expires_at", "must be in the future")
	}
	if err := v.Err(); err != nil {
		return IssuedKey{}, err
	}
	var out IssuedKey
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		m, err := q.GetMerchant(ctx, merchantID)
		if errors.Is(err, store.ErrNotFound) {
			return apperr.NotFound("merchant", merchantID)
		}
		if err != nil {
			return err
		}
		if m.Status == model.MerchantClosed {
			return apperr.New(apperr.CodeInvalidState, "cannot issue keys for a closed merchant")
		}
		out, err = s.issue(ctx, q, merchantID, in.Label, in.ExpiresAt, "", now)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, q, audit.Entry{
			Action: "api_key.create", ResourceType: "api_key", ResourceID: out.ID, MerchantID: merchantID,
			Metadata: map[string]any{"prefix": out.Prefix, "label": out.Label},
		})
	})
	return out, err
}

func (s *Service) issue(ctx context.Context, q *store.Queries, merchantID, label string, expires *time.Time, rotatedFrom string, now time.Time) (IssuedKey, error) {
	for attempt := 0; attempt < 3; attempt++ {
		prefix, full := newKeyMaterial()
		k := model.APIKey{
			ID: ids.New("key"), MerchantID: merchantID, Prefix: prefix, Hash: HashKey(full), Label: label,
			Status: model.APIKeyActive, RotatedFrom: rotatedFrom, CreatedAt: now, ExpiresAt: expires,
		}
		err := q.InsertAPIKey(ctx, k)
		if errors.Is(err, store.ErrConflict) {
			continue // astronomically unlikely prefix collision; retry
		}
		if err != nil {
			return IssuedKey{}, err
		}
		return IssuedKey{APIKey: k, Secret: full}, nil
	}
	return IssuedKey{}, errors.New("merchant: could not allocate a unique key prefix")
}

// RotateKey issues a replacement key and schedules the old one to expire
// after a grace period (zero grace = immediate revocation).
func (s *Service) RotateKey(ctx context.Context, merchantID, keyID string, grace *time.Duration) (IssuedKey, error) {
	g := s.opts.KeyRotationGrace
	if grace != nil {
		g = *grace
	}
	if g < 0 || g > 30*24*time.Hour {
		var v apperr.Validator
		v.Add("grace_period_seconds", "must be between 0 and 2592000")
		return IssuedKey{}, v.Err()
	}
	var out IssuedKey
	err := s.db.Tx(ctx, func(q *store.Queries) error {
		old, err := q.GetAPIKey(ctx, keyID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && old.MerchantID != merchantID) {
			return apperr.NotFound("api key", keyID)
		}
		if err != nil {
			return err
		}
		now := s.clock.Now()
		if !old.Usable(now) {
			return apperr.New(apperr.CodeInvalidState, "only active, unexpired keys can be rotated")
		}
		out, err = s.issue(ctx, q, merchantID, old.Label, nil, old.ID, now)
		if err != nil {
			return err
		}
		if g == 0 {
			err = q.RevokeAPIKey(ctx, old.ID, now)
		} else {
			exp := now.Add(g)
			if old.ExpiresAt != nil && old.ExpiresAt.Before(exp) {
				exp = *old.ExpiresAt
			}
			err = q.SetAPIKeyExpiry(ctx, old.ID, exp)
		}
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, q, audit.Entry{
			Action: "api_key.rotate", ResourceType: "api_key", ResourceID: old.ID, MerchantID: merchantID,
			Metadata: map[string]any{"new_key_id": out.ID, "new_prefix": out.Prefix, "grace_seconds": int64(g.Seconds())},
		})
	})
	return out, err
}

// RevokeKey immediately disables a key.
func (s *Service) RevokeKey(ctx context.Context, merchantID, keyID string) error {
	return s.db.Tx(ctx, func(q *store.Queries) error {
		k, err := q.GetAPIKey(ctx, keyID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && k.MerchantID != merchantID) {
			return apperr.NotFound("api key", keyID)
		}
		if err != nil {
			return err
		}
		if k.Status == model.APIKeyRevoked {
			return apperr.New(apperr.CodeInvalidState, "api key is already revoked")
		}
		if err := q.RevokeAPIKey(ctx, keyID, s.clock.Now()); err != nil {
			return err
		}
		return s.audit.Record(ctx, q, audit.Entry{
			Action: "api_key.revoke", ResourceType: "api_key", ResourceID: keyID, MerchantID: merchantID,
			Metadata: map[string]any{"prefix": k.Prefix},
		})
	})
}

// ListKeys lists a merchant's keys (hashes are never exposed).
func (s *Service) ListKeys(ctx context.Context, merchantID string) ([]model.APIKey, error) {
	if _, err := s.Get(ctx, merchantID); err != nil {
		return nil, err
	}
	return s.db.Q().ListAPIKeys(ctx, merchantID)
}

// errBadKey is deliberately generic: callers must not learn whether the
// prefix exists, the key expired, or the merchant is closed.
var errBadKey = apperr.New(apperr.CodeUnauthenticated, "invalid API key")

// Authenticate resolves a full API key to its key record and merchant.
func (s *Service) Authenticate(ctx context.Context, full string) (model.APIKey, model.Merchant, error) {
	prefix, ok := ParseKey(full)
	if !ok {
		return model.APIKey{}, model.Merchant{}, errBadKey
	}
	q := s.db.Q()
	k, err := q.GetAPIKeyByPrefix(ctx, prefix)
	if errors.Is(err, store.ErrNotFound) {
		return model.APIKey{}, model.Merchant{}, errBadKey
	}
	if err != nil {
		return model.APIKey{}, model.Merchant{}, err
	}
	if subtle.ConstantTimeCompare([]byte(HashKey(full)), []byte(k.Hash)) != 1 {
		return model.APIKey{}, model.Merchant{}, errBadKey
	}
	now := s.clock.Now()
	if !k.Usable(now) {
		return model.APIKey{}, model.Merchant{}, errBadKey
	}
	m, err := q.GetMerchant(ctx, k.MerchantID)
	if err != nil {
		return model.APIKey{}, model.Merchant{}, err
	}
	if m.Status == model.MerchantClosed {
		return model.APIKey{}, model.Merchant{}, errBadKey
	}
	if k.LastUsedAt == nil || now.Sub(*k.LastUsedAt) >= s.opts.APIKeyTouchResolution {
		if err := q.TouchAPIKey(ctx, k.ID, now); err != nil {
			return model.APIKey{}, model.Merchant{}, err
		}
	}
	return k, m, nil
}
