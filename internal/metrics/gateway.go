package metrics

// Gateway bundles the application's metric families so components receive
// one typed handle instead of string names.
type Gateway struct {
	Registry *Registry

	HTTPRequests     *CounterVec
	HTTPDuration     *HistogramVec
	PaymentOps       *CounterVec
	AcquirerLatency  *HistogramVec
	FraudDecisions   *CounterVec
	WebhookAttempts  *CounterVec
	WebhookLatency   *HistogramVec
	RateLimited      *CounterVec
	SettlementRuns   *CounterVec
	Reconciliations  *CounterVec
	IdempotentReplay *CounterVec
	JobRuns          *CounterVec
}

// NewGateway registers the gateway's metric families on a fresh registry.
func NewGateway() *Gateway {
	r := NewRegistry()
	return &Gateway{
		Registry: r,
		HTTPRequests: r.NewCounterVec("gateway_http_requests_total",
			"HTTP requests by route pattern, method and status class.", "route", "method", "status"),
		HTTPDuration: r.NewHistogramVec("gateway_http_request_duration_seconds",
			"HTTP request latency by route pattern.", DefBuckets, "route"),
		PaymentOps: r.NewCounterVec("gateway_payment_operations_total",
			"Payment lifecycle operations by action and resulting status.", "action", "status"),
		AcquirerLatency: r.NewHistogramVec("gateway_acquirer_latency_seconds",
			"Simulated acquirer round-trip latency by operation.", DefBuckets, "operation"),
		FraudDecisions: r.NewCounterVec("gateway_fraud_decisions_total",
			"Fraud engine verdicts by action.", "action"),
		WebhookAttempts: r.NewCounterVec("gateway_webhook_attempts_total",
			"Webhook delivery attempts by outcome.", "outcome"),
		WebhookLatency: r.NewHistogramVec("gateway_webhook_attempt_duration_seconds",
			"Webhook delivery attempt latency.", DefBuckets),
		RateLimited: r.NewCounterVec("gateway_rate_limited_total",
			"Requests rejected by the rate limiter by principal type.", "principal"),
		SettlementRuns: r.NewCounterVec("gateway_settlement_batches_total",
			"Settlement batches created by outcome.", "outcome"),
		Reconciliations: r.NewCounterVec("gateway_reconciliation_runs_total",
			"Reconciliation runs by result status.", "status"),
		IdempotentReplay: r.NewCounterVec("gateway_idempotent_replays_total",
			"Requests answered from the idempotency store.", "outcome"),
		JobRuns: r.NewCounterVec("gateway_job_runs_total",
			"Background job executions by job and result.", "job", "result"),
	}
}
