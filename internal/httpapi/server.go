// Package httpapi exposes the gateway's REST API: merchant endpoints under
// /v1 (API-key auth), operator endpoints under /admin/v1 (admin tokens),
// and health/metrics endpoints.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/robertramos/enterprise-payment-gateway/internal/acquirer"
	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/auth"
	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/fraud"
	"github.com/robertramos/enterprise-payment-gateway/internal/idempotency"
	"github.com/robertramos/enterprise-payment-gateway/internal/merchant"
	"github.com/robertramos/enterprise-payment-gateway/internal/metrics"
	"github.com/robertramos/enterprise-payment-gateway/internal/payment"
	"github.com/robertramos/enterprise-payment-gateway/internal/ratelimit"
	"github.com/robertramos/enterprise-payment-gateway/internal/reconcile"
	"github.com/robertramos/enterprise-payment-gateway/internal/settlement"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
	"github.com/robertramos/enterprise-payment-gateway/internal/webhook"
)

// Limiters groups the rate limiters per principal type.
type Limiters struct {
	Merchant *ratelimit.Limiter
	Admin    *ratelimit.Limiter
	Anon     *ratelimit.Limiter
}

// Job is an operator-triggerable background task.
type Job func(ctx context.Context) (any, error)

// Deps are the server's collaborators.
type Deps struct {
	DB          *store.DB
	Merchants   *merchant.Service
	Payments    *payment.Service
	Settlements *settlement.Service
	Recon       *reconcile.Service
	Outbox      *webhook.Outbox
	Idempotency *idempotency.Service
	Blocklist   *fraud.Blocklist
	Fraud       *fraud.Engine
	Vault       *vault.Service
	Acquirer    *acquirer.Acquirer
	Admins      *auth.Admins
	Metrics     *metrics.Gateway
	Limits      Limiters
	Jobs        map[string]Job
	Clock       clock.Clock
	Log         *slog.Logger
	MaxBody     int64
	TrustProxy  bool
	Version     string
}

// Server is the HTTP API.
type Server struct {
	db          *store.DB
	merchants   *merchant.Service
	payments    *payment.Service
	settlements *settlement.Service
	recon       *reconcile.Service
	outbox      *webhook.Outbox
	idem        *idempotency.Service
	blocklist   *fraud.Blocklist
	fraud       *fraud.Engine
	vault       *vault.Service
	acquirer    *acquirer.Acquirer
	admins      *auth.Admins
	metrics     *metrics.Gateway
	limits      Limiters
	jobs        map[string]Job
	clock       clock.Clock
	log         *slog.Logger
	maxBody     int64
	trustProxy  bool
	version     string
}

// New builds the server.
func New(d Deps) *Server {
	if d.MaxBody <= 0 {
		d.MaxBody = 1 << 20
	}
	return &Server{
		db: d.DB, merchants: d.Merchants, payments: d.Payments, settlements: d.Settlements, recon: d.Recon,
		outbox: d.Outbox, idem: d.Idempotency, blocklist: d.Blocklist, fraud: d.Fraud, vault: d.Vault,
		acquirer: d.Acquirer, admins: d.Admins, metrics: d.Metrics, limits: d.Limits, jobs: d.Jobs,
		clock: d.Clock, log: d.Log, maxBody: d.MaxBody, trustProxy: d.TrustProxy, version: d.Version,
	}
}

// Handler returns the routed, middleware-wrapped handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	return s.withRequestContext(s.withAccessLog(s.jsonFallback(mux)))
}

// jsonFallback renders the mux's plain-text 404/405 responses as JSON
// error envelopes.
func (s *Server) jsonFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &headerOnly{header: http.Header{}}
		mux.ServeHTTP(probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.header.Get("Allow"))
			s.writeError(w, r, apperr.New(apperr.CodeMethodNotAllowed, "method %s not allowed for %s", r.Method, r.URL.Path))
			return
		}
		s.writeError(w, r, apperr.New(apperr.CodeNotFound, "no route for %s %s", r.Method, r.URL.Path))
	})
}

// headerOnly is a ResponseWriter that records the status and discards the body.
type headerOnly struct {
	header http.Header
	status int
}

func (h *headerOnly) Header() http.Header         { return h.header }
func (h *headerOnly) Write(b []byte) (int, error) { return len(b), nil }
func (h *headerOnly) WriteHeader(code int)        { h.status = code }

func (s *Server) routes(mux *http.ServeMux) {
	// Operational endpoints.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /metrics", s.adminOnly(s.handleMetrics))
	mux.HandleFunc("GET /v1/test_cards", s.anonymous(s.handleTestCards))

	// Merchant API.
	m := s.merchantOnly
	mi := func(h http.HandlerFunc) http.HandlerFunc { return s.merchantOnly(s.idempotent(h)) }
	mux.HandleFunc("GET /v1/merchant", m(s.handleSelf))
	mux.HandleFunc("GET /v1/balance", m(s.handleSelfBalance))
	mux.HandleFunc("GET /v1/api_keys", m(s.handleSelfKeys))
	mux.HandleFunc("POST /v1/api_keys/rotate", mi(s.handleSelfRotateKey))
	mux.HandleFunc("POST /v1/tokens", mi(s.handleTokenize))
	mux.HandleFunc("POST /v1/payment_intents", mi(s.handleCreatePayment))
	mux.HandleFunc("GET /v1/payment_intents", m(s.handleListPayments))
	mux.HandleFunc("GET /v1/payment_intents/{id}", m(s.handleGetPayment))
	mux.HandleFunc("GET /v1/payment_intents/{id}/history", m(s.handlePaymentHistory))
	mux.HandleFunc("POST /v1/payment_intents/{id}/authorize", mi(s.handleAuthorize))
	mux.HandleFunc("POST /v1/payment_intents/{id}/3ds", mi(s.handleComplete3DS))
	mux.HandleFunc("POST /v1/payment_intents/{id}/capture", mi(s.handleCapture))
	mux.HandleFunc("POST /v1/payment_intents/{id}/void", mi(s.handleVoid))
	mux.HandleFunc("POST /v1/payment_intents/{id}/cancel", mi(s.handleCancel))
	mux.HandleFunc("POST /v1/payment_intents/{id}/refunds", mi(s.handleRefund))
	mux.HandleFunc("GET /v1/payment_intents/{id}/refunds", m(s.handleListRefunds))
	mux.HandleFunc("GET /v1/refunds/{id}", m(s.handleGetRefund))
	mux.HandleFunc("POST /v1/webhook_endpoints", mi(s.handleCreateEndpoint))
	mux.HandleFunc("GET /v1/webhook_endpoints", m(s.handleListEndpoints))
	mux.HandleFunc("GET /v1/webhook_endpoints/{id}", m(s.handleGetEndpoint))
	mux.HandleFunc("PATCH /v1/webhook_endpoints/{id}", m(s.handleUpdateEndpoint))
	mux.HandleFunc("POST /v1/webhook_endpoints/{id}/rotate_secret", m(s.handleRotateSecret))
	mux.HandleFunc("POST /v1/webhook_endpoints/{id}/ping", m(s.handlePingEndpoint))
	mux.HandleFunc("GET /v1/webhook_endpoints/{id}/deliveries", m(s.handleEndpointDeliveries))
	mux.HandleFunc("GET /v1/webhook_deliveries/{id}", m(s.handleGetDelivery))
	mux.HandleFunc("GET /v1/events", m(s.handleListEvents))
	mux.HandleFunc("GET /v1/events/{id}", m(s.handleGetEvent))
	mux.HandleFunc("POST /v1/events/{id}/replay", m(s.handleReplayEvent))
	mux.HandleFunc("GET /v1/settlements", m(s.handleListSettlements))
	mux.HandleFunc("GET /v1/settlements/{id}", m(s.handleGetSettlement))
	mux.HandleFunc("GET /v1/settlements/{id}/report", m(s.handleSettlementReport))
	mux.HandleFunc("GET /v1/payouts", m(s.handleListPayouts))

	// Operator API.
	a := s.adminOnly
	mux.HandleFunc("POST /admin/v1/merchants", a(s.handleOnboard))
	mux.HandleFunc("GET /admin/v1/merchants", a(s.handleListMerchants))
	mux.HandleFunc("GET /admin/v1/merchants/{id}", a(s.handleGetMerchant))
	mux.HandleFunc("PATCH /admin/v1/merchants/{id}", a(s.handleUpdateMerchant))
	mux.HandleFunc("POST /admin/v1/merchants/{id}/status", a(s.handleMerchantStatus))
	mux.HandleFunc("GET /admin/v1/merchants/{id}/fee_schedules", a(s.handleListFees))
	mux.HandleFunc("POST /admin/v1/merchants/{id}/fee_schedules", a(s.handleSetFees))
	mux.HandleFunc("GET /admin/v1/merchants/{id}/api_keys", a(s.handleListKeys))
	mux.HandleFunc("POST /admin/v1/merchants/{id}/api_keys", a(s.handleCreateKey))
	mux.HandleFunc("POST /admin/v1/merchants/{id}/api_keys/{key_id}/rotate", a(s.handleRotateKey))
	mux.HandleFunc("DELETE /admin/v1/merchants/{id}/api_keys/{key_id}", a(s.handleRevokeKey))
	mux.HandleFunc("GET /admin/v1/merchants/{id}/balance", a(s.handleMerchantBalance))
	mux.HandleFunc("GET /admin/v1/payment_intents", a(s.handleAdminListPayments))
	mux.HandleFunc("GET /admin/v1/payment_intents/{id}", a(s.handleAdminGetPayment))
	mux.HandleFunc("POST /admin/v1/payment_intents/{id}/review", a(s.handleReview))
	mux.HandleFunc("POST /admin/v1/settlements/run", a(s.handleRunSettlement))
	mux.HandleFunc("GET /admin/v1/settlements", a(s.handleAdminListSettlements))
	mux.HandleFunc("GET /admin/v1/settlements/summary", a(s.handleSettlementSummary))
	mux.HandleFunc("GET /admin/v1/settlements/{id}/report", a(s.handleAdminSettlementReport))
	mux.HandleFunc("POST /admin/v1/payouts/pay_due", a(s.handlePayDue))
	mux.HandleFunc("GET /admin/v1/acquirer/settlement_file", a(s.handleAcquirerFile))
	mux.HandleFunc("POST /admin/v1/reconciliations", a(s.handleReconcile))
	mux.HandleFunc("POST /admin/v1/reconciliations/upload", a(s.handleReconcileUpload))
	mux.HandleFunc("GET /admin/v1/reconciliations", a(s.handleListReconciliations))
	mux.HandleFunc("GET /admin/v1/reconciliations/{id}", a(s.handleGetReconciliation))
	mux.HandleFunc("GET /admin/v1/audit_logs", a(s.handleAuditLogs))
	mux.HandleFunc("GET /admin/v1/audit_logs/verify", a(s.handleAuditVerify))
	mux.HandleFunc("GET /admin/v1/fraud/blocklist", a(s.handleListBlocklist))
	mux.HandleFunc("POST /admin/v1/fraud/blocklist", a(s.handleAddBlocklist))
	mux.HandleFunc("DELETE /admin/v1/fraud/blocklist/{bin}", a(s.handleRemoveBlocklist))
	mux.HandleFunc("GET /admin/v1/fraud/rules", a(s.handleFraudRules))
	mux.HandleFunc("GET /admin/v1/vault/status", a(s.handleVaultStatus))
	mux.HandleFunc("POST /admin/v1/vault/rewrap", a(s.handleVaultRewrap))
	mux.HandleFunc("GET /admin/v1/ledger/trial_balance", a(s.handleTrialBalance))
	mux.HandleFunc("POST /admin/v1/jobs/{name}/run", a(s.handleRunJob))
}
