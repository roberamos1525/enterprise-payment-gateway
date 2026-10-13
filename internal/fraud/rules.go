package fraud

import (
	"context"
	"fmt"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/money"
)

// binBlocklistRule hard-blocks cards whose BIN is on the blocklist.
type binBlocklistRule struct {
	check BlocklistChecker
}

func (r *binBlocklistRule) Name() string { return "bin_blocklist" }

func (r *binBlocklistRule) Evaluate(ctx context.Context, in Input, _ time.Time) (*Signal, error) {
	if r.check == nil || in.BIN == "" {
		return nil, nil
	}
	blocked, reason, err := r.check(ctx, in.BIN)
	if err != nil || !blocked {
		return nil, err
	}
	return &Signal{Score: 100, Block: true, Detail: fmt.Sprintf("BIN %s is blocklisted (%s)", in.BIN, reason)}, nil
}

// velocityRule fires when a key exceeds max events in its window; at twice
// the limit it forces a block.
type velocityRule struct {
	name  string
	v     *Velocity
	max   int
	score int
	key   func(Input) string
}

func (r *velocityRule) Name() string { return r.name }

func (r *velocityRule) Evaluate(_ context.Context, in Input, now time.Time) (*Signal, error) {
	k := r.key(in)
	if k == "" {
		return nil, nil
	}
	n := r.v.Hit(k, now)
	if n <= r.max {
		return nil, nil
	}
	return &Signal{
		Score:  r.score,
		Block:  n > 2*r.max,
		Detail: fmt.Sprintf("%d attempts in %s (limit %d)", n, r.v.window, r.max),
	}, nil
}

// amountRule scores large amounts and hard-blocks very large ones.
type amountRule struct {
	cfg Config
}

func (r *amountRule) Name() string { return "amount_threshold" }

func (r *amountRule) Evaluate(_ context.Context, in Input, _ time.Time) (*Signal, error) {
	review, block := r.cfg.AmountReviewMinor, r.cfg.AmountBlockMinor
	if t, ok := r.cfg.AmountOverrides[in.Currency]; ok {
		review, block = t[0], t[1]
	}
	switch {
	case in.Amount >= block:
		return &Signal{Score: 100, Block: true, Detail: fmt.Sprintf("amount %s exceeds block threshold %s",
			money.FormatMinor(in.Amount, in.Currency), money.FormatMinor(block, in.Currency))}, nil
	case in.Amount >= review:
		return &Signal{Score: r.cfg.AmountScore, Detail: fmt.Sprintf("amount %s exceeds review threshold %s",
			money.FormatMinor(in.Amount, in.Currency), money.FormatMinor(review, in.Currency))}, nil
	}
	return nil, nil
}

// countryMismatchRule scores billing country != card issuing country.
type countryMismatchRule struct {
	score int
}

func (r *countryMismatchRule) Name() string { return "billing_country_mismatch" }

func (r *countryMismatchRule) Evaluate(_ context.Context, in Input, _ time.Time) (*Signal, error) {
	if in.BillingCountry == "" || in.CardCountry == "" || in.BillingCountry == in.CardCountry {
		return nil, nil
	}
	return &Signal{Score: r.score, Detail: fmt.Sprintf("billing country %s differs from card country %s",
		in.BillingCountry, in.CardCountry)}, nil
}

// ipCountryRule scores client IP geolocation != card issuing country.
type ipCountryRule struct {
	geo   *GeoIP
	score int
}

func (r *ipCountryRule) Name() string { return "ip_country_mismatch" }

func (r *ipCountryRule) Evaluate(_ context.Context, in Input, _ time.Time) (*Signal, error) {
	if r.geo == nil || in.IP == "" || in.CardCountry == "" {
		return nil, nil
	}
	ipc := r.geo.Country(in.IP)
	if ipc == "" || ipc == in.CardCountry {
		return nil, nil
	}
	return &Signal{Score: r.score, Detail: fmt.Sprintf("IP country %s differs from card country %s", ipc, in.CardCountry)}, nil
}

// highRiskCountryRule scores cards issued in or billed to configured countries.
type highRiskCountryRule struct {
	countries map[string]bool
	score     int
}

func (r *highRiskCountryRule) Name() string { return "high_risk_country" }

func (r *highRiskCountryRule) Evaluate(_ context.Context, in Input, _ time.Time) (*Signal, error) {
	for _, c := range []string{in.CardCountry, in.BillingCountry} {
		if c != "" && r.countries[c] {
			return &Signal{Score: r.score, Detail: fmt.Sprintf("country %s is configured as high risk", c)}, nil
		}
	}
	return nil, nil
}
