package fraud

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

func TestVelocityWindow(t *testing.T) {
	v := NewVelocity(time.Minute, 0)
	for i := 1; i <= 3; i++ {
		if n := v.Hit("card", now.Add(time.Duration(i)*time.Second)); n != i {
			t.Fatalf("hit %d returned %d", i, n)
		}
	}
	if n := v.Count("card", now.Add(61*time.Second)); n != 2 {
		t.Fatalf("after first expired: %d", n)
	}
	if n := v.Count("card", now.Add(5*time.Minute)); n != 0 {
		t.Fatalf("all expired: %d", n)
	}
	v.Hit("a", now)
	v.Hit("b", now)
	if live := v.Sweep(now.Add(2 * time.Minute)); live != 0 {
		t.Fatalf("sweep left %d keys", live)
	}
}

func TestVelocityMaxKeys(t *testing.T) {
	v := NewVelocity(time.Minute, 2)
	v.Hit("a", now)
	v.Hit("b", now)
	// Map is full and nothing has expired: the hit is counted but not stored.
	if n := v.Hit("c", now); n != 1 {
		t.Fatalf("overflow hit = %d", n)
	}
	if v.Count("c", now) != 0 {
		t.Fatal("overflow key should not be stored")
	}
	// Once old keys expire, the sweep frees room.
	if n := v.Hit("c", now.Add(2*time.Minute)); n != 1 || v.Count("c", now.Add(2*time.Minute)) != 1 {
		t.Fatal("key not stored after sweep")
	}
}

func TestGeoIP(t *testing.T) {
	g, err := NewGeoIP(map[string]string{"203.0.113.0/24": "de", "203.0.113.128/25": "AT"})
	if err != nil {
		t.Fatal(err)
	}
	if g.Country("203.0.113.5") != "DE" || g.Country("203.0.113.200") != "AT" {
		t.Fatal("longest prefix should win")
	}
	if g.Country("8.8.8.8") != "" || g.Country("not-an-ip") != "" {
		t.Fatal("unknown addresses must map to empty")
	}
	if _, err := NewGeoIP(map[string]string{"bad": "US"}); err == nil {
		t.Fatal("bad cidr accepted")
	}
	if _, err := NewGeoIP(map[string]string{"10.0.0.0/8": "USA"}); err == nil {
		t.Fatal("bad country accepted")
	}
}

func engine(t *testing.T, mutate func(*Config), blocked map[string]bool) *Engine {
	t.Helper()
	cfg := DefaultConfig()
	cfg.CardVelocityMax, cfg.IPVelocityMax = 2, 3
	cfg.HighRiskCountries = []string{"KP"}
	if mutate != nil {
		mutate(&cfg)
	}
	geo, _ := NewGeoIP(DefaultGeoRanges)
	check := func(_ context.Context, bin string) (bool, string, error) {
		if bin == "999999" {
			return false, "", errors.New("db down")
		}
		return blocked[bin], "test", nil
	}
	e, err := NewEngine(cfg, geo, check)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func base() Input {
	return Input{MerchantID: "mer_1", CardFingerprint: "fp_1", BIN: "424242", CardCountry: "US",
		BillingCountry: "US", IP: "192.0.2.1", Amount: 1000, Currency: "USD"}
}

func TestEngineAllowsCleanTraffic(t *testing.T) {
	d, err := engine(t, nil, nil).Evaluate(context.Background(), base(), now)
	if err != nil || d.Action != Allow || d.Score != 0 || len(d.Signals) != 0 {
		t.Fatalf("decision = %+v %v", d, err)
	}
}

func TestEngineRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Input)
		action Action
		rule   string
	}{
		{"blocklisted bin", func(in *Input) { in.BIN = "410000" }, Block, "bin_blocklist"},
		{"review amount", func(in *Input) { in.Amount = 600_000 }, Review, "amount_threshold"},
		{"block amount", func(in *Input) { in.Amount = 6_000_000 }, Block, "amount_threshold"},
		{"billing mismatch", func(in *Input) { in.BillingCountry = "BR" }, Allow, "billing_country_mismatch"},
		{"ip mismatch", func(in *Input) { in.IP = "198.51.100.7" }, Allow, "ip_country_mismatch"},
		{"both mismatches", func(in *Input) { in.BillingCountry = "BR"; in.IP = "203.0.113.9" }, Allow, "billing_country_mismatch"},
		{"high risk + mismatch", func(in *Input) { in.BillingCountry = "KP"; in.IP = "203.0.113.9" }, Review, "high_risk_country"},
		{"currency override", func(in *Input) { in.Currency = "JPY"; in.Amount = 20_000 }, Review, "amount_threshold"},
	}
	for _, c := range cases {
		e := engine(t, func(cfg *Config) { cfg.AmountOverrides = map[string][2]int64{"JPY": {10_000, 100_000}} },
			map[string]bool{"410000": true})
		in := base()
		c.mutate(&in)
		d, err := e.Evaluate(context.Background(), in, now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		found := false
		for _, s := range d.Signals {
			found = found || s.Rule == c.rule
		}
		if d.Action != c.action || !found {
			t.Errorf("%s: action=%s signals=%+v", c.name, d.Action, d.Signals)
		}
	}
}

func TestEngineVelocityEscalates(t *testing.T) {
	e := engine(t, nil, nil)
	var actions []Action
	for i := 0; i < 5; i++ {
		in := base()
		in.IP = "" // isolate the card rule
		d, _ := e.Evaluate(context.Background(), in, now.Add(time.Duration(i)*time.Second))
		actions = append(actions, d.Action)
	}
	// limit 2: hits 3-4 score 40 (allow), hit 5 exceeds 2x limit -> block.
	want := []Action{Allow, Allow, Allow, Allow, Block}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("actions = %v, want %v", actions, want)
		}
	}
	if live := e.Sweep(now.Add(time.Hour)); live != 0 {
		t.Fatalf("sweep left %d", live)
	}
}

func TestEngineReasonsAndErrors(t *testing.T) {
	e := engine(t, nil, nil)
	in := base()
	in.Amount = 700_000
	in.BillingCountry = "DE"
	d, _ := e.Evaluate(context.Background(), in, now)
	reasons := d.Reasons()
	if len(reasons) != 2 || !strings.HasPrefix(reasons[0], "amount_threshold(+50)") {
		t.Fatalf("reasons sorted by score = %v", reasons)
	}
	in = base()
	in.BIN = "999999"
	if _, err := e.Evaluate(context.Background(), in, now); err == nil {
		t.Fatal("blocklist failure must surface")
	}
}

func TestDisabledRulesAndConfigValidation(t *testing.T) {
	e := engine(t, func(c *Config) { c.DisabledRules = []string{"amount_threshold", "card_velocity"} }, nil)
	for _, r := range e.RuleNames() {
		if r == "amount_threshold" || r == "card_velocity" {
			t.Fatalf("disabled rule %s still active", r)
		}
	}
	in := base()
	in.Amount = 9_000_000
	if d, _ := e.Evaluate(context.Background(), in, now); d.Action != Allow {
		t.Fatalf("disabled amount rule fired: %+v", d)
	}
	bad := []func(*Config){
		func(c *Config) { c.ReviewScore, c.BlockScore = 80, 50 },
		func(c *Config) { c.AmountReviewMinor = 0 },
		func(c *Config) { c.CardVelocityMax = 0 },
		func(c *Config) { c.AmountOverrides = map[string][2]int64{"USD": {10, 5}} },
	}
	for i, m := range bad {
		cfg := DefaultConfig()
		m(&cfg)
		if _, err := NewEngine(cfg, nil, nil); err == nil {
			t.Errorf("bad config %d accepted", i)
		}
	}
}
