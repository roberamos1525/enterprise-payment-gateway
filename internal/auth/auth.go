// Package auth authenticates API callers. Merchants use API keys (verified
// by the merchant service); operators use admin bearer tokens configured via
// the environment. Only SHA-256 digests of admin tokens are kept in memory.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

// AdminToken is a named operator credential.
type AdminToken struct {
	Name   string
	digest [32]byte
}

// Admins verifies admin bearer tokens.
type Admins struct {
	tokens []AdminToken
}

var adminNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

// ParseAdmins parses "name:token[,name:token...]". Tokens must be at least
// 24 characters so weak placeholders cannot be deployed by accident.
func ParseAdmins(spec string) (*Admins, error) {
	a := &Admins{}
	seen := map[string]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, token, ok := strings.Cut(part, ":")
		if !ok || !adminNameRe.MatchString(name) {
			return nil, fmt.Errorf("auth: admin token entries must be name:token with a lowercase name")
		}
		if len(token) < 24 {
			return nil, fmt.Errorf("auth: admin token %q must be at least 24 characters", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("auth: duplicate admin token name %q", name)
		}
		seen[name] = true
		a.tokens = append(a.tokens, AdminToken{Name: name, digest: sha256.Sum256([]byte(token))})
	}
	if len(a.tokens) == 0 {
		return nil, errors.New("auth: at least one admin token is required")
	}
	return a, nil
}

// Authenticate returns the admin name for a token. Every configured token is
// compared in constant time so timing does not reveal which one matched.
func (a *Admins) Authenticate(token string) (string, bool) {
	d := sha256.Sum256([]byte(token))
	name := ""
	for _, t := range a.tokens {
		if subtle.ConstantTimeCompare(d[:], t.digest[:]) == 1 {
			name = t.Name
		}
	}
	return name, name != ""
}

// Count returns the number of configured admin tokens.
func (a *Admins) Count() int { return len(a.tokens) }

// BearerToken extracts the token from an Authorization header value.
func BearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

type ctxKey int

const keyMerchant ctxKey = iota

// WithMerchant stores the authenticated merchant and key on the context.
func WithMerchant(ctx context.Context, m model.Merchant, k model.APIKey) context.Context {
	return context.WithValue(ctx, keyMerchant, MerchantPrincipal{Merchant: m, Key: k})
}

// MerchantPrincipal is an authenticated merchant caller.
type MerchantPrincipal struct {
	Merchant model.Merchant
	Key      model.APIKey
}

// MerchantFrom returns the authenticated merchant, if any.
func MerchantFrom(ctx context.Context) (MerchantPrincipal, bool) {
	p, ok := ctx.Value(keyMerchant).(MerchantPrincipal)
	return p, ok
}
