// Package config loads gateway configuration from defaults, an optional JSON
// file and GATEWAY_* environment variables (highest precedence), then
// validates the result.
package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/money"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
)

// Duration is a time.Duration that (un)marshals as a Go duration string.
type Duration struct{ time.Duration }

// UnmarshalJSON accepts "1m30s" style strings.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalJSON renders the duration string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// D is shorthand for building a Duration.
func D(d time.Duration) Duration { return Duration{d} }

// Config is the full gateway configuration.
type Config struct {
	Env string `json:"env"` // dev | prod

	ListenAddr        string   `json:"listen_addr"`
	ReadTimeout       Duration `json:"read_timeout"`
	WriteTimeout      Duration `json:"write_timeout"`
	ShutdownTimeout   Duration `json:"shutdown_timeout"`
	MaxBodyBytes      int64    `json:"max_body_bytes"`
	TrustProxyHeaders bool     `json:"trust_proxy_headers"`

	DBPath    string `json:"db_path"`
	LogLevel  string `json:"log_level"`
	LogFormat string `json:"log_format"`

	// Secrets: environment only (never read from the config file).
	VaultKeys         string `json:"-"`
	FingerprintSecret string `json:"-"`
	AdminTokens       string `json:"-"`
	EphemeralVault    bool   `json:"-"`

	RateLimit RateLimitConfig `json:"rate_limit"`
	Payments  PaymentsConfig  `json:"payments"`
	Acquirer  AcquirerConfig  `json:"acquirer"`
	Fraud     FraudConfig     `json:"fraud"`
	Fees      FeesConfig      `json:"fees"`
	Settle    SettleConfig    `json:"settlement"`
	Webhooks  WebhooksConfig  `json:"webhooks"`
	Keys      KeysConfig      `json:"api_keys"`
	Jobs      JobsConfig      `json:"jobs"`
}

// RateLimitConfig sets token-bucket parameters per principal type.
type RateLimitConfig struct {
	MerchantRPS   float64 `json:"merchant_rps"`
	MerchantBurst int     `json:"merchant_burst"`
	AdminRPS      float64 `json:"admin_rps"`
	AdminBurst    int     `json:"admin_burst"`
	AnonRPS       float64 `json:"anonymous_rps"`
	AnonBurst     int     `json:"anonymous_burst"`
}

// PaymentsConfig tunes the payment lifecycle.
type PaymentsConfig struct {
	AuthorizationTTL Duration `json:"authorization_ttl"`
	MaxAmountMinor   int64    `json:"max_amount_minor"`
	AcquirerTimeout  Duration `json:"acquirer_timeout"`
	IdempotencyTTL   Duration `json:"idempotency_ttl"`
}

// AcquirerConfig tunes the simulator.
type AcquirerConfig struct {
	BaseLatency     Duration `json:"base_latency"`
	Jitter          Duration `json:"jitter"`
	SlowLatency     Duration `json:"slow_latency"`
	DiscrepancyRate float64  `json:"discrepancy_rate"`
	DiscrepancySeed string   `json:"discrepancy_seed"`
}

// FraudConfig tunes the risk engine.
type FraudConfig struct {
	ReviewScore            int                 `json:"review_score"`
	BlockScore             int                 `json:"block_score"`
	AmountReviewMinor      int64               `json:"amount_review_minor"`
	AmountBlockMinor       int64               `json:"amount_block_minor"`
	AmountOverrides        map[string][2]int64 `json:"amount_overrides"`
	CardVelocityMax        int                 `json:"card_velocity_max"`
	CardVelocityWindow     Duration            `json:"card_velocity_window"`
	IPVelocityMax          int                 `json:"ip_velocity_max"`
	IPVelocityWindow       Duration            `json:"ip_velocity_window"`
	MerchantVelocityMax    int                 `json:"merchant_velocity_max"`
	MerchantVelocityWindow Duration            `json:"merchant_velocity_window"`
	BlockedBINs            []string            `json:"blocked_bins"`
	HighRiskCountries      []string            `json:"high_risk_countries"`
	DisabledRules          []string            `json:"disabled_rules"`
	GeoRanges              map[string]string   `json:"geo_ranges"`
}

// FeesConfig is the default fee schedule for new merchants.
type FeesConfig struct {
	PercentBps        int64  `json:"percent_bps"`
	FixedMinor        int64  `json:"fixed_minor"`
	RefundFixedMinor  int64  `json:"refund_fixed_minor"`
	ReturnFeeOnRefund bool   `json:"return_fee_on_refund"`
	Rounding          string `json:"rounding"`
}

// SettleConfig tunes settlement.
type SettleConfig struct {
	PayoutDelayDays int  `json:"payout_delay_days"`
	AutoRun         bool `json:"auto_run"`
	AutoRunHourUTC  int  `json:"auto_run_hour_utc"`
}

// WebhooksConfig tunes delivery.
type WebhooksConfig struct {
	AllowInsecureHTTP    bool     `json:"allow_insecure_http"`
	AllowPrivateNetworks bool     `json:"allow_private_networks"`
	MaxAttempts          int      `json:"max_attempts"`
	BaseBackoff          Duration `json:"base_backoff"`
	MaxBackoff           Duration `json:"max_backoff"`
	RequestTimeout       Duration `json:"request_timeout"`
	PollInterval         Duration `json:"poll_interval"`
	Concurrency          int      `json:"concurrency"`
	SecretRotationGrace  Duration `json:"secret_rotation_grace"`
	MaxEndpoints         int      `json:"max_endpoints_per_merchant"`
}

// KeysConfig tunes API key handling.
type KeysConfig struct {
	RotationGrace Duration `json:"rotation_grace"`
}

// JobsConfig sets background job intervals.
type JobsConfig struct {
	ExpiryInterval Duration `json:"authorization_expiry_interval"`
	PurgeInterval  Duration `json:"purge_interval"`
	PayoutInterval Duration `json:"payout_interval"`
}

// Default returns the default configuration (secrets unset).
func Default() Config {
	return Config{
		Env: "prod", ListenAddr: "127.0.0.1:8080",
		ReadTimeout: D(15 * time.Second), WriteTimeout: D(30 * time.Second), ShutdownTimeout: D(15 * time.Second),
		MaxBodyBytes: 1 << 20, DBPath: "gateway.db", LogLevel: "info", LogFormat: "json",
		RateLimit: RateLimitConfig{MerchantRPS: 25, MerchantBurst: 50, AdminRPS: 10, AdminBurst: 20, AnonRPS: 5, AnonBurst: 10},
		Payments: PaymentsConfig{AuthorizationTTL: D(7 * 24 * time.Hour), MaxAmountMinor: 100_000_000,
			AcquirerTimeout: D(10 * time.Second), IdempotencyTTL: D(24 * time.Hour)},
		Acquirer: AcquirerConfig{BaseLatency: D(20 * time.Millisecond), Jitter: D(30 * time.Millisecond),
			SlowLatency: D(1500 * time.Millisecond), DiscrepancySeed: "epg"},
		Fraud: FraudConfig{ReviewScore: 50, BlockScore: 80, AmountReviewMinor: 500_000, AmountBlockMinor: 5_000_000,
			CardVelocityMax: 5, CardVelocityWindow: D(10 * time.Minute), IPVelocityMax: 10, IPVelocityWindow: D(10 * time.Minute),
			MerchantVelocityMax: 600, MerchantVelocityWindow: D(time.Minute), BlockedBINs: []string{"410000"}},
		Fees:   FeesConfig{PercentBps: 290, FixedMinor: 30, Rounding: "half_even"},
		Settle: SettleConfig{PayoutDelayDays: 2, AutoRunHourUTC: 1},
		Webhooks: WebhooksConfig{MaxAttempts: 8, BaseBackoff: D(5 * time.Second), MaxBackoff: D(time.Hour),
			RequestTimeout: D(10 * time.Second), PollInterval: D(500 * time.Millisecond), Concurrency: 8,
			SecretRotationGrace: D(24 * time.Hour), MaxEndpoints: 10},
		Keys: KeysConfig{RotationGrace: D(24 * time.Hour)},
		Jobs: JobsConfig{ExpiryInterval: D(time.Minute), PurgeInterval: D(10 * time.Minute), PayoutInterval: D(5 * time.Minute)},
	}
}

// Load builds the configuration: defaults, then the JSON file at path (if
// non-empty), then environment variables from getenv.
func Load(path string, getenv func(string) string) (Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	if err := applyEnv(&cfg, getenv); err != nil {
		return cfg, err
	}
	if cfg.Env == "dev" && cfg.VaultKeys == "" && cfg.FingerprintSecret == "" {
		// Development convenience only: data sealed with an ephemeral key
		// becomes unreadable after restart. Refused outside dev.
		cfg.VaultKeys = "dev:" + base64.StdEncoding.EncodeToString(vault.GenerateKey())
		cfg.FingerprintSecret = base64.StdEncoding.EncodeToString(vault.GenerateKey())
		cfg.EphemeralVault = true
	}
	return cfg, cfg.Validate()
}

// Validate checks the configuration for consistency.
func (c Config) Validate() error {
	var errs []string
	bad := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if c.Env != "dev" && c.Env != "prod" {
		bad("env must be dev or prod")
	}
	if c.ListenAddr == "" {
		bad("listen_addr is required")
	}
	if c.DBPath == "" {
		bad("db_path is required")
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 64<<20 {
		bad("max_body_bytes must be between 1KiB and 64MiB")
	}
	if c.VaultKeys == "" {
		bad("GATEWAY_VAULT_KEYS is required (id:base64-32-byte-key[,...])")
	} else if _, err := vault.ParseKeyring(c.VaultKeys, c.FingerprintSecret); err != nil {
		bad("vault: %v", err)
	}
	if c.AdminTokens == "" {
		bad("GATEWAY_ADMIN_TOKENS is required (name:token[,...])")
	}
	if c.RateLimit.MerchantRPS <= 0 || c.RateLimit.AdminRPS <= 0 || c.RateLimit.AnonRPS <= 0 {
		bad("rate limits must be positive")
	}
	if c.Payments.MaxAmountMinor <= 0 || c.Payments.AuthorizationTTL.Duration <= 0 {
		bad("payments.max_amount_minor and authorization_ttl must be positive")
	}
	if c.Acquirer.DiscrepancyRate < 0 || c.Acquirer.DiscrepancyRate > 1 {
		bad("acquirer.discrepancy_rate must be within [0,1]")
	}
	if c.Fees.PercentBps < 0 || c.Fees.PercentBps > 10000 || c.Fees.FixedMinor < 0 || c.Fees.RefundFixedMinor < 0 {
		bad("fees are out of range")
	}
	if _, err := money.ParseRounding(c.Fees.Rounding); err != nil {
		bad("%v", err)
	}
	if c.Settle.PayoutDelayDays < 0 || c.Settle.PayoutDelayDays > 30 {
		bad("settlement.payout_delay_days must be 0-30")
	}
	if c.Settle.AutoRunHourUTC < 0 || c.Settle.AutoRunHourUTC > 23 {
		bad("settlement.auto_run_hour_utc must be 0-23")
	}
	if c.Webhooks.MaxAttempts < 1 || c.Webhooks.MaxAttempts > 50 {
		bad("webhooks.max_attempts must be 1-50")
	}
	for _, b := range c.Fraud.BlockedBINs {
		if len(b) != 6 || strings.Trim(b, "0123456789") != "" {
			bad("fraud.blocked_bins entries must be 6 digits (got %q)", b)
		}
	}
	if len(errs) > 0 {
		return errors.New("config: " + strings.Join(errs, "; "))
	}
	return nil
}

type envBinding struct {
	name  string
	apply func(string) error
}

func str(dst *string) func(string) error { return func(v string) error { *dst = v; return nil } }

func boolean(dst *bool) func(string) error {
	return func(v string) error {
		b, err := strconv.ParseBool(v)
		*dst = b
		return err
	}
}

func integer(dst *int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		*dst = n
		return err
	}
}

func int64v(dst *int64) func(string) error {
	return func(v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		*dst = n
		return err
	}
}

func float(dst *float64) func(string) error {
	return func(v string) error {
		f, err := strconv.ParseFloat(v, 64)
		*dst = f
		return err
	}
}

func dur(dst *Duration) func(string) error {
	return func(v string) error {
		d, err := time.ParseDuration(v)
		dst.Duration = d
		return err
	}
}

func list(dst *[]string) func(string) error {
	return func(v string) error {
		*dst = nil
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				*dst = append(*dst, s)
			}
		}
		return nil
	}
}

func applyEnv(c *Config, getenv func(string) string) error {
	bindings := []envBinding{
		{"GATEWAY_ENV", str(&c.Env)},
		{"GATEWAY_LISTEN_ADDR", str(&c.ListenAddr)},
		{"GATEWAY_READ_TIMEOUT", dur(&c.ReadTimeout)},
		{"GATEWAY_WRITE_TIMEOUT", dur(&c.WriteTimeout)},
		{"GATEWAY_SHUTDOWN_TIMEOUT", dur(&c.ShutdownTimeout)},
		{"GATEWAY_MAX_BODY_BYTES", int64v(&c.MaxBodyBytes)},
		{"GATEWAY_TRUST_PROXY_HEADERS", boolean(&c.TrustProxyHeaders)},
		{"GATEWAY_DB_PATH", str(&c.DBPath)},
		{"GATEWAY_LOG_LEVEL", str(&c.LogLevel)},
		{"GATEWAY_LOG_FORMAT", str(&c.LogFormat)},
		{"GATEWAY_VAULT_KEYS", str(&c.VaultKeys)},
		{"GATEWAY_FINGERPRINT_SECRET", str(&c.FingerprintSecret)},
		{"GATEWAY_ADMIN_TOKENS", str(&c.AdminTokens)},
		{"GATEWAY_RATE_MERCHANT_RPS", float(&c.RateLimit.MerchantRPS)},
		{"GATEWAY_RATE_MERCHANT_BURST", integer(&c.RateLimit.MerchantBurst)},
		{"GATEWAY_RATE_ADMIN_RPS", float(&c.RateLimit.AdminRPS)},
		{"GATEWAY_RATE_ADMIN_BURST", integer(&c.RateLimit.AdminBurst)},
		{"GATEWAY_RATE_ANON_RPS", float(&c.RateLimit.AnonRPS)},
		{"GATEWAY_RATE_ANON_BURST", integer(&c.RateLimit.AnonBurst)},
		{"GATEWAY_AUTH_TTL", dur(&c.Payments.AuthorizationTTL)},
		{"GATEWAY_MAX_AMOUNT_MINOR", int64v(&c.Payments.MaxAmountMinor)},
		{"GATEWAY_ACQUIRER_TIMEOUT", dur(&c.Payments.AcquirerTimeout)},
		{"GATEWAY_IDEMPOTENCY_TTL", dur(&c.Payments.IdempotencyTTL)},
		{"GATEWAY_ACQUIRER_LATENCY", dur(&c.Acquirer.BaseLatency)},
		{"GATEWAY_ACQUIRER_JITTER", dur(&c.Acquirer.Jitter)},
		{"GATEWAY_ACQUIRER_SLOW_LATENCY", dur(&c.Acquirer.SlowLatency)},
		{"GATEWAY_ACQUIRER_DISCREPANCY_RATE", float(&c.Acquirer.DiscrepancyRate)},
		{"GATEWAY_ACQUIRER_DISCREPANCY_SEED", str(&c.Acquirer.DiscrepancySeed)},
		{"GATEWAY_FRAUD_REVIEW_SCORE", integer(&c.Fraud.ReviewScore)},
		{"GATEWAY_FRAUD_BLOCK_SCORE", integer(&c.Fraud.BlockScore)},
		{"GATEWAY_FRAUD_AMOUNT_REVIEW_MINOR", int64v(&c.Fraud.AmountReviewMinor)},
		{"GATEWAY_FRAUD_AMOUNT_BLOCK_MINOR", int64v(&c.Fraud.AmountBlockMinor)},
		{"GATEWAY_FRAUD_CARD_VELOCITY_MAX", integer(&c.Fraud.CardVelocityMax)},
		{"GATEWAY_FRAUD_IP_VELOCITY_MAX", integer(&c.Fraud.IPVelocityMax)},
		{"GATEWAY_FRAUD_BLOCKED_BINS", list(&c.Fraud.BlockedBINs)},
		{"GATEWAY_FRAUD_HIGH_RISK_COUNTRIES", list(&c.Fraud.HighRiskCountries)},
		{"GATEWAY_FRAUD_DISABLED_RULES", list(&c.Fraud.DisabledRules)},
		{"GATEWAY_FEE_PERCENT_BPS", int64v(&c.Fees.PercentBps)},
		{"GATEWAY_FEE_FIXED_MINOR", int64v(&c.Fees.FixedMinor)},
		{"GATEWAY_FEE_REFUND_FIXED_MINOR", int64v(&c.Fees.RefundFixedMinor)},
		{"GATEWAY_FEE_RETURN_ON_REFUND", boolean(&c.Fees.ReturnFeeOnRefund)},
		{"GATEWAY_FEE_ROUNDING", str(&c.Fees.Rounding)},
		{"GATEWAY_PAYOUT_DELAY_DAYS", integer(&c.Settle.PayoutDelayDays)},
		{"GATEWAY_SETTLEMENT_AUTO_RUN", boolean(&c.Settle.AutoRun)},
		{"GATEWAY_SETTLEMENT_HOUR_UTC", integer(&c.Settle.AutoRunHourUTC)},
		{"GATEWAY_WEBHOOK_ALLOW_HTTP", boolean(&c.Webhooks.AllowInsecureHTTP)},
		{"GATEWAY_WEBHOOK_ALLOW_PRIVATE", boolean(&c.Webhooks.AllowPrivateNetworks)},
		{"GATEWAY_WEBHOOK_MAX_ATTEMPTS", integer(&c.Webhooks.MaxAttempts)},
		{"GATEWAY_WEBHOOK_BASE_BACKOFF", dur(&c.Webhooks.BaseBackoff)},
		{"GATEWAY_WEBHOOK_MAX_BACKOFF", dur(&c.Webhooks.MaxBackoff)},
		{"GATEWAY_WEBHOOK_TIMEOUT", dur(&c.Webhooks.RequestTimeout)},
		{"GATEWAY_WEBHOOK_POLL_INTERVAL", dur(&c.Webhooks.PollInterval)},
		{"GATEWAY_WEBHOOK_SECRET_GRACE", dur(&c.Webhooks.SecretRotationGrace)},
		{"GATEWAY_KEY_ROTATION_GRACE", dur(&c.Keys.RotationGrace)},
	}
	for _, b := range bindings {
		v := strings.TrimSpace(getenv(b.name))
		if v == "" {
			continue
		}
		if err := b.apply(v); err != nil {
			return fmt.Errorf("config: %s: invalid value: %w", b.name, err)
		}
	}
	return nil
}
