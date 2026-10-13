package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func secrets() map[string]string {
	return map[string]string{
		"GATEWAY_VAULT_KEYS":         "k1:" + base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"GATEWAY_FINGERPRINT_SECRET": "fingerprint-secret-0123",
		"GATEWAY_ADMIN_TOKENS":       "ops:0123456789abcdef0123456789",
	}
}

func TestDefaultsRequireSecrets(t *testing.T) {
	_, err := Load("", env(nil))
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_VAULT_KEYS") || !strings.Contains(err.Error(), "GATEWAY_ADMIN_TOKENS") {
		t.Fatalf("missing secrets: %v", err)
	}
	cfg, err := Load("", env(secrets()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != "prod" || cfg.ListenAddr != "127.0.0.1:8080" || cfg.Webhooks.AllowPrivateNetworks || cfg.EphemeralVault {
		t.Fatalf("unsafe defaults: %+v", cfg)
	}
}

func TestEnvOverrides(t *testing.T) {
	e := secrets()
	e["GATEWAY_LISTEN_ADDR"] = ":9999"
	e["GATEWAY_AUTH_TTL"] = "48h"
	e["GATEWAY_FRAUD_BLOCKED_BINS"] = "411111, 422222"
	e["GATEWAY_WEBHOOK_ALLOW_PRIVATE"] = "true"
	e["GATEWAY_RATE_MERCHANT_RPS"] = "2.5"
	e["GATEWAY_FEE_PERCENT_BPS"] = "150"
	cfg, err := Load("", env(e))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":9999" || cfg.Payments.AuthorizationTTL.Duration != 48*time.Hour ||
		len(cfg.Fraud.BlockedBINs) != 2 || cfg.Fraud.BlockedBINs[1] != "422222" || !cfg.Webhooks.AllowPrivateNetworks ||
		cfg.RateLimit.MerchantRPS != 2.5 || cfg.Fees.PercentBps != 150 {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	e["GATEWAY_AUTH_TTL"] = "soon"
	if _, err := Load("", env(e)); err == nil || !strings.Contains(err.Error(), "GATEWAY_AUTH_TTL") {
		t.Fatalf("bad duration: %v", err)
	}
}

func TestConfigFileAndPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gw.json")
	body := `{"listen_addr": "0.0.0.0:7000", "log_level": "debug",
		"payments": {"authorization_ttl": "72h", "max_amount_minor": 5000, "acquirer_timeout": "3s", "idempotency_ttl": "1h"},
		"fraud": {"review_score": 40, "block_score": 90, "amount_review_minor": 100, "amount_block_minor": 200,
			"card_velocity_max": 3, "card_velocity_window": "5m", "ip_velocity_max": 9, "ip_velocity_window": "5m",
			"merchant_velocity_max": 99, "merchant_velocity_window": "1m", "geo_ranges": {"10.0.0.0/8": "NL"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	e := secrets()
	e["GATEWAY_LISTEN_ADDR"] = "127.0.0.1:7001" // env wins over file
	cfg, err := Load(path, env(e))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:7001" || cfg.LogLevel != "debug" || cfg.Payments.MaxAmountMinor != 5000 ||
		cfg.Payments.AuthorizationTTL.Duration != 72*time.Hour || cfg.Fraud.GeoRanges["10.0.0.0/8"] != "NL" {
		t.Fatalf("file config: %+v", cfg)
	}
	if err := os.WriteFile(path, []byte(`{"listen_adr": "typo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, env(e)); err == nil {
		t.Fatal("unknown config keys must be rejected")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json"), env(e)); err == nil {
		t.Fatal("missing file accepted")
	}
	b, _ := D(90 * time.Second).MarshalJSON()
	if string(b) != `"1m30s"` {
		t.Fatalf("duration json = %s", b)
	}
}

func TestDevModeEphemeralVault(t *testing.T) {
	e := map[string]string{"GATEWAY_ENV": "dev", "GATEWAY_ADMIN_TOKENS": "ops:0123456789abcdef0123456789"}
	cfg, err := Load("", env(e))
	if err != nil || !cfg.EphemeralVault || cfg.VaultKeys == "" {
		t.Fatalf("dev ephemeral: %+v %v", cfg, err)
	}
	e["GATEWAY_ENV"] = "prod"
	if _, err := Load("", env(e)); err == nil {
		t.Fatal("prod must not generate keys")
	}
}

func TestValidationRules(t *testing.T) {
	mutations := map[string]func(*Config){
		"env":           func(c *Config) { c.Env = "staging" },
		"body":          func(c *Config) { c.MaxBodyBytes = 10 },
		"rate":          func(c *Config) { c.RateLimit.AdminRPS = 0 },
		"discrepancy":   func(c *Config) { c.Acquirer.DiscrepancyRate = 1.5 },
		"fees":          func(c *Config) { c.Fees.PercentBps = 20_000 },
		"rounding":      func(c *Config) { c.Fees.Rounding = "up" },
		"payout delay":  func(c *Config) { c.Settle.PayoutDelayDays = 99 },
		"settle hour":   func(c *Config) { c.Settle.AutoRunHourUTC = 24 },
		"attempts":      func(c *Config) { c.Webhooks.MaxAttempts = 0 },
		"blocked bins":  func(c *Config) { c.Fraud.BlockedBINs = []string{"12ab56"} },
		"bad vault key": func(c *Config) { c.VaultKeys = "k1:short" },
	}
	for name, m := range mutations {
		cfg, err := Load("", env(secrets()))
		if err != nil {
			t.Fatal(err)
		}
		m(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: invalid config accepted", name)
		}
	}
}
