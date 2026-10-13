// Package fraud implements a rules-based risk engine. Each rule contributes
// a score (and may force a block); the total score maps to an action:
// allow, review or block.
package fraud

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Action is the engine's verdict.
type Action string

// Actions.
const (
	Allow  Action = "allow"
	Review Action = "review"
	Block  Action = "block"
)

// Input is what the engine knows about an authorization attempt.
type Input struct {
	MerchantID      string
	CardFingerprint string
	BIN             string
	CardCountry     string
	BillingCountry  string
	IP              string
	Amount          int64
	Currency        string
}

// Signal is one rule's contribution to a decision.
type Signal struct {
	Rule   string `json:"rule"`
	Score  int    `json:"score"`
	Block  bool   `json:"block,omitempty"`
	Detail string `json:"detail"`
}

// Decision is the result of evaluating all rules.
type Decision struct {
	Score   int      `json:"score"`
	Action  Action   `json:"action"`
	Signals []Signal `json:"signals"`
}

// Reasons returns human-readable reasons for storage on the payment.
func (d Decision) Reasons() []string {
	out := make([]string, 0, len(d.Signals))
	for _, s := range d.Signals {
		out = append(out, fmt.Sprintf("%s(+%d): %s", s.Rule, s.Score, s.Detail))
	}
	return out
}

// Rule evaluates one risk signal. A nil signal means the rule did not fire.
type Rule interface {
	Name() string
	Evaluate(ctx context.Context, in Input, now time.Time) (*Signal, error)
}

// Config holds thresholds and rule parameters.
type Config struct {
	ReviewScore int
	BlockScore  int

	CardVelocityWindow time.Duration
	CardVelocityMax    int
	IPVelocityWindow   time.Duration
	IPVelocityMax      int
	MerchantWindow     time.Duration
	MerchantMax        int
	VelocityScore      int

	AmountReviewMinor int64               // default threshold in minor units
	AmountBlockMinor  int64               // default hard-block threshold
	AmountOverrides   map[string][2]int64 // currency -> {review, block}
	AmountScore       int

	CountryMismatchScore   int
	IPCountryMismatchScore int
	HighRiskCountries      []string
	HighRiskCountryScore   int

	DisabledRules []string
}

// DefaultConfig returns conservative defaults.
func DefaultConfig() Config {
	return Config{
		ReviewScore: 50, BlockScore: 80,
		CardVelocityWindow: 10 * time.Minute, CardVelocityMax: 5,
		IPVelocityWindow: 10 * time.Minute, IPVelocityMax: 10,
		MerchantWindow: time.Minute, MerchantMax: 600,
		VelocityScore:     40,
		AmountReviewMinor: 500_000, AmountBlockMinor: 5_000_000, AmountScore: 50,
		CountryMismatchScore: 25, IPCountryMismatchScore: 15,
		HighRiskCountryScore: 30,
	}
}

// Validate checks threshold sanity.
func (c Config) Validate() error {
	switch {
	case c.ReviewScore <= 0 || c.BlockScore <= c.ReviewScore:
		return fmt.Errorf("fraud: need 0 < review score < block score")
	case c.AmountReviewMinor <= 0 || c.AmountBlockMinor <= c.AmountReviewMinor:
		return fmt.Errorf("fraud: need 0 < amount review threshold < amount block threshold")
	case c.CardVelocityMax <= 0 || c.IPVelocityMax <= 0 || c.MerchantMax <= 0:
		return fmt.Errorf("fraud: velocity limits must be positive")
	}
	for cur, t := range c.AmountOverrides {
		if t[0] <= 0 || t[1] <= t[0] {
			return fmt.Errorf("fraud: bad amount thresholds for %s", cur)
		}
	}
	return nil
}

// Engine runs the configured rules.
type Engine struct {
	cfg   Config
	rules []Rule
	velos []*Velocity
}

// BlocklistChecker reports whether a BIN is blocked.
type BlocklistChecker func(ctx context.Context, bin string) (bool, string, error)

// NewEngine builds the engine with the standard rule set.
func NewEngine(cfg Config, geo *GeoIP, blocked BlocklistChecker) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	card := NewVelocity(cfg.CardVelocityWindow, 0)
	ip := NewVelocity(cfg.IPVelocityWindow, 0)
	mer := NewVelocity(cfg.MerchantWindow, 0)
	all := []Rule{
		&binBlocklistRule{check: blocked},
		&velocityRule{name: "card_velocity", v: card, max: cfg.CardVelocityMax, score: cfg.VelocityScore,
			key: func(in Input) string { return in.CardFingerprint }},
		&velocityRule{name: "ip_velocity", v: ip, max: cfg.IPVelocityMax, score: cfg.VelocityScore,
			key: func(in Input) string { return in.IP }},
		&velocityRule{name: "merchant_velocity", v: mer, max: cfg.MerchantMax, score: cfg.VelocityScore / 2,
			key: func(in Input) string { return in.MerchantID }},
		&amountRule{cfg: cfg},
		&countryMismatchRule{score: cfg.CountryMismatchScore},
		&ipCountryRule{geo: geo, score: cfg.IPCountryMismatchScore},
		&highRiskCountryRule{countries: setOf(cfg.HighRiskCountries), score: cfg.HighRiskCountryScore},
	}
	disabled := setOf(cfg.DisabledRules)
	e := &Engine{cfg: cfg, velos: []*Velocity{card, ip, mer}}
	for _, r := range all {
		if !disabled[r.Name()] {
			e.rules = append(e.rules, r)
		}
	}
	return e, nil
}

func setOf(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[strings.ToUpper(strings.TrimSpace(s))] = true
		m[strings.TrimSpace(s)] = true
	}
	return m
}

// RuleNames lists the active rules.
func (e *Engine) RuleNames() []string {
	out := make([]string, len(e.rules))
	for i, r := range e.rules {
		out[i] = r.Name()
	}
	return out
}

// Config returns the engine configuration.
func (e *Engine) Config() Config { return e.cfg }

// Evaluate scores an attempt. Velocity rules also record the attempt.
func (e *Engine) Evaluate(ctx context.Context, in Input, now time.Time) (Decision, error) {
	d := Decision{Action: Allow, Signals: []Signal{}}
	forced := false
	for _, r := range e.rules {
		s, err := r.Evaluate(ctx, in, now)
		if err != nil {
			return Decision{}, fmt.Errorf("fraud: rule %s: %w", r.Name(), err)
		}
		if s == nil {
			continue
		}
		s.Rule = r.Name()
		d.Signals = append(d.Signals, *s)
		d.Score += s.Score
		forced = forced || s.Block
	}
	sort.SliceStable(d.Signals, func(i, j int) bool { return d.Signals[i].Score > d.Signals[j].Score })
	if d.Score > 100 {
		d.Score = 100
	}
	switch {
	case forced || d.Score >= e.cfg.BlockScore:
		d.Action = Block
	case d.Score >= e.cfg.ReviewScore:
		d.Action = Review
	}
	return d, nil
}

// Sweep prunes velocity state (called periodically by a background job).
func (e *Engine) Sweep(now time.Time) int {
	n := 0
	for _, v := range e.velos {
		n += v.Sweep(now)
	}
	return n
}
