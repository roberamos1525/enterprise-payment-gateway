// Package app wires the gateway together from configuration and owns its
// lifecycle (HTTP server, webhook dispatcher, background jobs).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/acquirer"
	"github.com/robertramos/enterprise-payment-gateway/internal/audit"
	"github.com/robertramos/enterprise-payment-gateway/internal/auth"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/internal/fraud"
	"github.com/robertramos/enterprise-payment-gateway/internal/httpapi"
	"github.com/robertramos/enterprise-payment-gateway/internal/idempotency"
	"github.com/robertramos/enterprise-payment-gateway/internal/jobs"
	"github.com/robertramos/enterprise-payment-gateway/internal/ledger"
	"github.com/robertramos/enterprise-payment-gateway/internal/merchant"
	"github.com/robertramos/enterprise-payment-gateway/internal/metrics"
	"github.com/robertramos/enterprise-payment-gateway/internal/money"
	"github.com/robertramos/enterprise-payment-gateway/internal/payment"
	"github.com/robertramos/enterprise-payment-gateway/internal/ratelimit"
	"github.com/robertramos/enterprise-payment-gateway/internal/reconcile"
	"github.com/robertramos/enterprise-payment-gateway/internal/settlement"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

// Options carries runtime dependencies that are not configuration.
type Options struct {
	Clock   clock.Clock // nil = real clock
	Log     *slog.Logger
	Version string
}

// App is a fully wired gateway.
type App struct {
	Cfg         config.Config
	DB          *store.DB
	Clock       clock.Clock
	Log         *slog.Logger
	Metrics     *metrics.Gateway
	Keyring     *vault.Keyring
	Merchants   *merchant.Service
	Payments    *payment.Service
	Settlements *settlement.Service
	Recon       *reconcile.Service
	Dispatcher  *webhook.Dispatcher
	Fraud       *fraud.Engine
	Blocklist   *fraud.Blocklist
	Idempotency *idempotency.Service
	Jobs        *jobs.Runner
	Limits      httpapi.Limiters
	Handler     http.Handler

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New opens the database and builds every component.
func New(ctx context.Context, cfg config.Config, opts Options) (*App, error) {
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	kr, err := vault.ParseKeyring(cfg.VaultKeys, cfg.FingerprintSecret)
	if err != nil {
		return nil, err
	}
	admins, err := auth.ParseAdmins(cfg.AdminTokens)
	if err != nil {
		return nil, err
	}
	rounding, err := money.ParseRounding(cfg.Fees.Rounding)
	if err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, err
	}
	a := &App{Cfg: cfg, DB: db, Clock: clk, Log: log, Metrics: metrics.NewGateway(), Keyring: kr}
	if err := a.wire(ctx, admins, rounding, opts.Version); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

func (a *App) wire(ctx context.Context, admins *auth.Admins, rounding money.Rounding, version string) error {
	cfg, clk, log, m := a.Cfg, a.Clock, a.Log, a.Metrics
	al := audit.NewLogger(clk)
	outbox := webhook.NewOutbox(clk)
	vlt := vault.NewService(a.Keyring, clk)
	ldg := ledger.New(clk)
	policy := webhook.URLPolicy{AllowInsecureHTTP: cfg.Webhooks.AllowInsecureHTTP, AllowPrivateNetworks: cfg.Webhooks.AllowPrivateNetworks}

	a.Merchants = merchant.NewService(a.DB, al, clk, a.Keyring, merchant.Options{
		DefaultFee: merchant.FeeInput{PercentBps: cfg.Fees.PercentBps, FixedMinor: cfg.Fees.FixedMinor,
			RefundFixedMinor: cfg.Fees.RefundFixedMinor, ReturnFeeOnRefund: cfg.Fees.ReturnFeeOnRefund},
		KeyRotationGrace: cfg.Keys.RotationGrace.Duration, SecretRotationGrace: cfg.Webhooks.SecretRotationGrace.Duration,
		MaxEndpoints: cfg.Webhooks.MaxEndpoints, WebhookURLPolicy: policy,
	})

	a.Blocklist = fraud.NewBlocklist(a.DB, al, clk)
	if err := a.Blocklist.Seed(ctx, cfg.Fraud.BlockedBINs); err != nil {
		return fmt.Errorf("app: seed blocklist: %w", err)
	}
	geoRanges := fraud.DefaultGeoRanges
	if len(cfg.Fraud.GeoRanges) > 0 {
		geoRanges = cfg.Fraud.GeoRanges
	}
	geo, err := fraud.NewGeoIP(geoRanges)
	if err != nil {
		return err
	}
	fc := fraud.DefaultConfig()
	fc.ReviewScore, fc.BlockScore = cfg.Fraud.ReviewScore, cfg.Fraud.BlockScore
	fc.AmountReviewMinor, fc.AmountBlockMinor = cfg.Fraud.AmountReviewMinor, cfg.Fraud.AmountBlockMinor
	fc.AmountOverrides = map[string][2]int64{}
	for cur, t := range cfg.Fraud.AmountOverrides {
		fc.AmountOverrides[strings.ToUpper(cur)] = t
	}
	fc.CardVelocityMax, fc.CardVelocityWindow = cfg.Fraud.CardVelocityMax, cfg.Fraud.CardVelocityWindow.Duration
	fc.IPVelocityMax, fc.IPVelocityWindow = cfg.Fraud.IPVelocityMax, cfg.Fraud.IPVelocityWindow.Duration
	fc.MerchantMax, fc.MerchantWindow = cfg.Fraud.MerchantVelocityMax, cfg.Fraud.MerchantVelocityWindow.Duration
	fc.HighRiskCountries, fc.DisabledRules = cfg.Fraud.HighRiskCountries, cfg.Fraud.DisabledRules
	if a.Fraud, err = fraud.NewEngine(fc, geo, a.Blocklist.Check); err != nil {
		return err
	}

	acq := acquirer.New(a.DB, vlt, clk, acquirer.Config{
		BaseLatency: cfg.Acquirer.BaseLatency.Duration, Jitter: cfg.Acquirer.Jitter.Duration,
		SlowLatency: cfg.Acquirer.SlowLatency.Duration, DiscrepancyRate: cfg.Acquirer.DiscrepancyRate,
		DiscrepancySeed: cfg.Acquirer.DiscrepancySeed,
	}, m)
	a.Payments = payment.NewService(payment.Deps{
		DB: a.DB, Vault: vlt, Acquirer: acq, Fraud: a.Fraud, Ledger: ldg, Outbox: outbox, Audit: al,
		Clock: clk, Metrics: m, Log: log,
	}, payment.Options{AuthorizationTTL: cfg.Payments.AuthorizationTTL.Duration,
		MaxAmountMinor: cfg.Payments.MaxAmountMinor, AcquirerTimeout: cfg.Payments.AcquirerTimeout.Duration})
	a.Settlements = settlement.NewService(a.DB, ldg, outbox, al, clk, m, log,
		settlement.Options{Rounding: rounding, PayoutDelayDays: cfg.Settle.PayoutDelayDays})
	a.Recon = reconcile.NewService(a.DB, acq, al, clk, m)
	a.Idempotency = idempotency.NewService(a.DB, clk, idempotency.Options{TTL: cfg.Payments.IdempotencyTTL.Duration})
	a.Dispatcher = webhook.NewDispatcher(a.DB, a.Keyring, clk, webhook.DispatchOptions{
		PollInterval: cfg.Webhooks.PollInterval.Duration, Concurrency: cfg.Webhooks.Concurrency,
		RequestTimeout: cfg.Webhooks.RequestTimeout.Duration, MaxAttempts: cfg.Webhooks.MaxAttempts,
		BaseBackoff: cfg.Webhooks.BaseBackoff.Duration, MaxBackoff: cfg.Webhooks.MaxBackoff.Duration,
		Jitter: true, AllowPrivate: cfg.Webhooks.AllowPrivateNetworks, UserAgent: "enterprise-payment-gateway/" + version,
	}, log, m)
	a.Limits = httpapi.Limiters{
		Merchant: ratelimit.New(cfg.RateLimit.MerchantRPS, cfg.RateLimit.MerchantBurst, nil),
		Admin:    ratelimit.New(cfg.RateLimit.AdminRPS, cfg.RateLimit.AdminBurst, nil),
		Anon:     ratelimit.New(cfg.RateLimit.AnonRPS, cfg.RateLimit.AnonBurst, nil),
	}
	a.registerJobs()
	a.registerGauges()

	jobFns := map[string]httpapi.Job{}
	for _, name := range a.Jobs.Names() {
		jobFns[name] = func(ctx context.Context) (any, error) { return a.Jobs.RunNow(ctx, name) }
	}
	srv := httpapi.New(httpapi.Deps{
		DB: a.DB, Merchants: a.Merchants, Payments: a.Payments, Settlements: a.Settlements, Recon: a.Recon,
		Outbox: outbox, Idempotency: a.Idempotency, Blocklist: a.Blocklist, Fraud: a.Fraud, Vault: vlt,
		Acquirer: acq, Admins: admins, Metrics: m, Limits: a.Limits, Jobs: jobFns, Clock: clk, Log: log,
		MaxBody: cfg.MaxBodyBytes, TrustProxy: cfg.TrustProxyHeaders, Version: version,
	})
	a.Handler = srv.Handler()
	return nil
}

func (a *App) registerJobs() {
	a.Jobs = jobs.NewRunner(a.Log, a.Metrics)
	a.Jobs.Register(jobs.Job{Name: "expire_authorizations", Interval: a.Cfg.Jobs.ExpiryInterval.Duration,
		Run: func(ctx context.Context) (any, error) {
			n, err := a.Payments.ExpireAuthorizations(ctx, 200)
			return map[string]int{"expired": n}, err
		}})
	a.Jobs.Register(jobs.Job{Name: "purge_idempotency", Interval: a.Cfg.Jobs.PurgeInterval.Duration,
		Run: func(ctx context.Context) (any, error) {
			n, err := a.Idempotency.Purge(ctx)
			swept := a.Fraud.Sweep(a.Clock.Now())
			a.Limits.Merchant.Sweep()
			a.Limits.Admin.Sweep()
			a.Limits.Anon.Sweep()
			return map[string]int64{"idempotency_purged": n, "velocity_keys_live": int64(swept)}, err
		}})
	a.Jobs.Register(jobs.Job{Name: "pay_payouts", Interval: a.Cfg.Jobs.PayoutInterval.Duration,
		Run: func(ctx context.Context) (any, error) {
			n, err := a.Settlements.PayDuePayouts(ctx)
			return map[string]int{"paid": n}, err
		}})
	a.Jobs.Register(jobs.Job{Name: "dispatch_webhooks", Run: func(ctx context.Context) (any, error) {
		n, err := a.Dispatcher.RunOnce(ctx)
		return map[string]int{"attempted": n}, err
	}})
	settleInterval := time.Duration(0)
	if a.Cfg.Settle.AutoRun {
		settleInterval = 15 * time.Minute
	}
	a.Jobs.Register(jobs.Job{Name: "settle_previous_day", Interval: settleInterval,
		Run: func(ctx context.Context) (any, error) {
			now := a.Clock.Now()
			if now.Hour() < a.Cfg.Settle.AutoRunHourUTC {
				return map[string]string{"skipped": "before configured settlement hour"}, nil
			}
			return a.Settlements.Run(ctx, settlement.RunInput{Day: now.AddDate(0, 0, -1)})
		}})
}

func (a *App) registerGauges() {
	q := func(fn func(ctx context.Context) (map[string]float64, error)) func() map[string]float64 {
		return func() map[string]float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, err := fn(ctx)
			if err != nil {
				a.Log.Warn("metrics gauge query failed", "error", err)
				return map[string]float64{}
			}
			return out
		}
	}
	a.Metrics.Registry.NewGaugeFunc("gateway_payment_intents", "Payment intents by current status.", "status",
		q(func(ctx context.Context) (map[string]float64, error) {
			counts, err := a.DB.Q().CountPaymentsByStatus(ctx)
			out := map[string]float64{}
			for k, v := range counts {
				out[string(k)] = float64(v)
			}
			return out, err
		}))
	a.Metrics.Registry.NewGaugeFunc("gateway_webhook_deliveries", "Webhook deliveries by current status.", "status",
		q(func(ctx context.Context) (map[string]float64, error) {
			counts, err := a.DB.Q().CountDeliveriesByStatus(ctx)
			out := map[string]float64{}
			for k, v := range counts {
				out[string(k)] = float64(v)
			}
			return out, err
		}))
}

// Start launches the webhook dispatcher and periodic jobs.
func (a *App) Start(ctx context.Context) {
	ctx, a.cancel = context.WithCancel(ctx)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.Dispatcher.Run(ctx)
	}()
	a.Jobs.Start(ctx)
}

// Stop halts background work and closes the database.
func (a *App) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	a.Jobs.Wait()
	return a.DB.Close()
}

// Serve runs the HTTP server on ln until ctx is cancelled, then shuts down
// gracefully within the configured timeout.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           a.Handler,
		ReadTimeout:       a.Cfg.ReadTimeout.Duration,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      a.Cfg.WriteTimeout.Duration,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(a.Log.Handler(), slog.LevelWarn),
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), a.Cfg.ShutdownTimeout.Duration)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("app: shutdown: %w", err)
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
