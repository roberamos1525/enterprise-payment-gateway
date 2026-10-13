package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/clock"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/metrics"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
	"github.com/robertramos/enterprise-payment-gateway/internal/store"
	"github.com/robertramos/enterprise-payment-gateway/internal/vault"
)

// DispatchOptions tunes the dispatcher.
type DispatchOptions struct {
	PollInterval   time.Duration
	BatchSize      int
	Concurrency    int
	Lease          time.Duration
	RequestTimeout time.Duration
	MaxAttempts    int
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	Jitter         bool
	AllowPrivate   bool
	UserAgent      string
}

func (o *DispatchOptions) defaults() {
	if o.PollInterval <= 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 50
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 8
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 10 * time.Second
	}
	if o.Lease <= 0 {
		o.Lease = o.RequestTimeout + 30*time.Second
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 8
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 5 * time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = time.Hour
	}
	if o.UserAgent == "" {
		o.UserAgent = "enterprise-payment-gateway-webhooks/1.0"
	}
}

// Dispatcher delivers pending webhook deliveries.
type Dispatcher struct {
	db      *store.DB
	kr      *vault.Keyring
	clock   clock.Clock
	client  *http.Client
	opts    DispatchOptions
	log     *slog.Logger
	metrics *metrics.Gateway
}

// NewDispatcher builds a dispatcher whose HTTP client refuses to connect to
// non-public addresses unless AllowPrivate is set.
func NewDispatcher(db *store.DB, kr *vault.Keyring, clk clock.Clock, opts DispatchOptions, log *slog.Logger, m *metrics.Gateway) *Dispatcher {
	opts.defaults()
	guard := dialGuard(opts.AllowPrivate)
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error { return guard(network, address) },
	}
	transport := &http.Transport{
		Proxy:                 nil, // never route webhook traffic through an environment proxy
		DialContext:           dialer.DialContext,
		MaxIdleConns:          50,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: opts.RequestTimeout,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   opts.RequestTimeout,
		// Redirects are not followed: a redirect could point at an internal host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Dispatcher{db: db, kr: kr, clock: clk, client: client, opts: opts, log: log, metrics: m}
}

// Run polls for due deliveries until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(d.opts.PollInterval)
	defer t.Stop()
	for {
		if _, err := d.RunOnce(ctx); err != nil && ctx.Err() == nil {
			d.log.Error("webhook dispatch pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce claims one batch of due deliveries and attempts them concurrently.
// It returns the number of attempts made.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	var batch []model.Delivery
	err := d.db.Tx(ctx, func(q *store.Queries) error {
		var err error
		batch, err = q.ClaimDueDeliveries(ctx, d.clock.Now(), d.opts.Lease, d.opts.BatchSize)
		return err
	})
	if err != nil || len(batch) == 0 {
		return 0, err
	}
	sem := make(chan struct{}, d.opts.Concurrency)
	var wg sync.WaitGroup
	for _, del := range batch {
		wg.Add(1)
		sem <- struct{}{}
		go func(del model.Delivery) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := d.attempt(ctx, del); err != nil {
				d.log.Error("webhook attempt bookkeeping failed", "delivery_id", del.ID, "error", err)
			}
		}(del)
	}
	wg.Wait()
	return len(batch), nil
}

// Backoff returns the delay before attempt n+1 after n failed attempts:
// base*2^(n-1), capped at max, optionally with +/-20% jitter.
func Backoff(n int, base, max time.Duration, jitter bool) time.Duration {
	if n < 1 {
		n = 1
	}
	f := float64(base) * math.Pow(2, float64(n-1))
	if f > float64(max) {
		f = float64(max)
	}
	if jitter {
		f *= 0.8 + 0.4*rand.Float64()
	}
	return time.Duration(f)
}

type attemptResult struct {
	statusCode int
	err        string
	snippet    string
	duration   time.Duration
	final      bool // failure that must not be retried
}

func (d *Dispatcher) attempt(ctx context.Context, del model.Delivery) error {
	q := d.db.Q()
	res := d.send(ctx, q, del)
	now := d.clock.Now()
	del.Attempts++
	del.LastStatusCode = res.statusCode
	del.LastError = res.err
	del.UpdatedAt = now
	outcome := "retry"
	switch {
	case res.err == "" && res.statusCode >= 200 && res.statusCode < 300:
		del.Status, del.CompletedAt, outcome = model.DeliverySucceeded, &now, "success"
	case res.final || del.Attempts >= d.opts.MaxAttempts:
		del.Status, del.CompletedAt, outcome = model.DeliveryFailed, &now, "failed"
	default:
		del.NextAttemptAt = now.Add(Backoff(del.Attempts, d.opts.BaseBackoff, d.opts.MaxBackoff, d.opts.Jitter))
	}
	if d.metrics != nil {
		d.metrics.WebhookAttempts.With(outcome).Inc()
		d.metrics.WebhookLatency.Observe(res.duration.Seconds())
	}
	d.log.Info("webhook attempt", "delivery_id", del.ID, "event_id", del.EventID, "endpoint_id", del.EndpointID,
		"attempt", del.Attempts, "status_code", res.statusCode, "outcome", outcome, "error", res.err)
	return d.db.Tx(ctx, func(q *store.Queries) error {
		if err := q.InsertAttempt(ctx, model.DeliveryAttempt{
			ID: ids.New("wha"), DeliveryID: del.ID, Attempt: del.Attempts, StatusCode: res.statusCode,
			DurationMS: res.duration.Milliseconds(), Error: res.err, ResponseSnippet: res.snippet, CreatedAt: now,
		}); err != nil {
			return err
		}
		return q.UpdateDeliveryResult(ctx, del)
	})
}

func (d *Dispatcher) send(ctx context.Context, q *store.Queries, del model.Delivery) attemptResult {
	ev, err := q.GetEvent(ctx, del.EventID)
	if err != nil {
		return attemptResult{err: "event not found", final: true}
	}
	ep, err := q.GetEndpoint(ctx, del.EndpointID)
	if err != nil {
		return attemptResult{err: "endpoint not found", final: true}
	}
	if ep.Status != model.EndpointEnabled {
		return attemptResult{err: "endpoint disabled", final: true}
	}
	secrets, err := d.secrets(ep)
	if err != nil {
		return attemptResult{err: "signing secret unavailable", final: true}
	}
	body, err := EnvelopeFor(ev)
	if err != nil {
		return attemptResult{err: "cannot encode event", final: true}
	}
	reqCtx, cancel := context.WithTimeout(ctx, d.opts.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		return attemptResult{err: "invalid endpoint url", final: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", d.opts.UserAgent)
	req.Header.Set(HeaderEventID, ev.ID)
	req.Header.Set(HeaderEventType, ev.Type)
	req.Header.Set(HeaderDeliveryID, del.ID)
	req.Header.Set(HeaderAttempt, strconv.Itoa(del.Attempts+1))
	req.Header.Set(HeaderSignature, SignatureHeader(d.clock.Now().Unix(), body, secrets...))
	start := time.Now()
	resp, err := d.client.Do(req)
	dur := time.Since(start)
	if err != nil {
		return attemptResult{err: classify(err), duration: dur}
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // drain for connection reuse
	r := attemptResult{statusCode: resp.StatusCode, snippet: string(snippet), duration: dur}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		r.err = fmt.Sprintf("endpoint responded %d", resp.StatusCode)
	}
	return r
}

// secrets returns the current signing secret and, during a rotation grace
// window, the previous one.
func (d *Dispatcher) secrets(ep model.WebhookEndpoint) ([]string, error) {
	cur, err := d.kr.OpenString(ep.SecretSealed, SecretAAD(ep.ID))
	if err != nil {
		return nil, err
	}
	out := []string{cur}
	if ep.PrevSecretSealed != "" && ep.PrevSecretExpiresAt != nil && d.clock.Now().Before(*ep.PrevSecretExpiresAt) {
		if prev, err := d.kr.OpenString(ep.PrevSecretSealed, SecretAAD(ep.ID)); err == nil {
			out = append(out, prev)
		}
	}
	return out, nil
}

// SecretAAD is the additional authenticated data binding a sealed signing
// secret to its endpoint.
func SecretAAD(endpointID string) string { return "whsec|" + endpointID }

func classify(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	}
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
