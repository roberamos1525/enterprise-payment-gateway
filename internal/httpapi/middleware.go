package httpapi

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/auth"
	"github.com/robertramos/enterprise-payment-gateway/internal/ids"
	"github.com/robertramos/enterprise-payment-gateway/internal/ratelimit"
	"github.com/robertramos/enterprise-payment-gateway/internal/reqctx"
)

// statusRecorder captures the status code (and optionally the body) written
// by a handler.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	capture bool
	body    bytes.Buffer
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.capture {
		r.body.Write(b)
	}
	return r.ResponseWriter.Write(b)
}

// withRequestContext assigns a request id, resolves the client IP, applies
// security headers and the body size limit.
func (s *Server) withRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-Id")
		if rid == "" || len(rid) > 64 || strings.ContainsAny(rid, " \t\r\n") {
			rid = ids.New("req")
		}
		ctx := reqctx.WithRequestID(r.Context(), rid)
		ctx = reqctx.WithClientIP(ctx, s.clientIP(r))
		h := w.Header()
		h.Set("X-Request-Id", rid)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBody)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// clientIP returns the caller address; X-Forwarded-For is honoured only when
// the deployment declares a trusted reverse proxy.
func (s *Server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if net.ParseIP(first) != nil {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// withAccessLog records metrics and an access log line per request, and
// converts panics into 500 responses.
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		info := &accessInfo{actor: "anonymous"}
		r = r.WithContext(context.WithValue(r.Context(), accessKey{}, info))
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic serving request", "request_id", reqctx.RequestID(r.Context()), "panic", p)
				if rec.status == 0 {
					s.writeError(rec, r, apperr.New(apperr.CodeInternal, "internal error"))
				}
			}
			d := time.Since(start)
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			route := routeOf(r)
			s.metrics.HTTPRequests.With(route, r.Method, strconv.Itoa(status/100)+"xx").Inc()
			s.metrics.HTTPDuration.Observe(d.Seconds(), route)
			s.log.Info("http request", append(logAttrs(r, status, d), "actor", info.actor)...)
		}()
		next.ServeHTTP(rec, r)
	})
}

// accessInfo is filled in by inner middleware (auth) and read by the
// access logger, which only sees the outer request.
type accessInfo struct{ actor string }

type accessKey struct{}

func setAccessActor(r *http.Request, actor string) {
	if info, ok := r.Context().Value(accessKey{}).(*accessInfo); ok {
		info.actor = actor
	}
}

// principal kinds for rate limiting.
const (
	kindMerchant = "merchant"
	kindAdmin    = "admin"
	kindAnon     = "anonymous"
)

// limit applies the token bucket for the principal and sets the
// X-RateLimit-* headers. It returns false after writing a 429.
func (s *Server) limit(w http.ResponseWriter, r *http.Request, kind, key string) bool {
	var l *ratelimit.Limiter
	switch kind {
	case kindMerchant:
		l = s.limits.Merchant
	case kindAdmin:
		l = s.limits.Admin
	default:
		l = s.limits.Anon
	}
	d := l.Allow(kind + ":" + key)
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
	h.Set("X-RateLimit-Reset", strconv.Itoa(int(d.Reset.Seconds()+0.999)))
	if d.Allowed {
		return true
	}
	s.metrics.RateLimited.With(kind).Inc()
	h.Set("Retry-After", strconv.Itoa(int(d.RetryAfter.Seconds()+0.999)))
	s.writeError(w, r, apperr.New(apperr.CodeRateLimited, "rate limit exceeded; retry after %s", d.RetryAfter.Round(time.Millisecond)))
	return false
}

// merchantOnly authenticates a merchant API key and rate-limits per key.
// Failed authentications are rate-limited per client IP to slow brute force.
func (s *Server) merchantOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := auth.BearerToken(r.Header.Get("Authorization"))
		if !ok {
			if !s.limit(w, r, kindAnon, reqctx.ClientIP(r.Context())) {
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="merchant"`)
			s.writeError(w, r, apperr.New(apperr.CodeUnauthenticated, "missing bearer API key"))
			return
		}
		key, m, err := s.merchants.Authenticate(r.Context(), token)
		if err != nil {
			if !s.limit(w, r, kindAnon, reqctx.ClientIP(r.Context())) {
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="merchant"`)
			s.writeError(w, r, err)
			return
		}
		if !s.limit(w, r, kindMerchant, key.ID) {
			return
		}
		setAccessActor(r, "merchant:"+key.ID)
		ctx := auth.WithMerchant(r.Context(), m, key)
		ctx = reqctx.WithActor(ctx, reqctx.Actor{Type: reqctx.ActorMerchant, ID: key.ID, MerchantID: m.ID})
		next(w, r.WithContext(ctx))
	}
}

// adminOnly authenticates an operator token.
func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := auth.BearerToken(r.Header.Get("Authorization"))
		name := ""
		if ok {
			name, ok = s.admins.Authenticate(token)
		}
		if !ok {
			if !s.limit(w, r, kindAnon, reqctx.ClientIP(r.Context())) {
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
			s.writeError(w, r, apperr.New(apperr.CodeUnauthenticated, "invalid or missing admin token"))
			return
		}
		if !s.limit(w, r, kindAdmin, name) {
			return
		}
		setAccessActor(r, "admin:"+name)
		next(w, r.WithContext(reqctx.WithActor(r.Context(), reqctx.Actor{Type: reqctx.ActorAdmin, ID: name})))
	}
}

// anonymous rate-limits unauthenticated public endpoints per client IP.
func (s *Server) anonymous(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limit(w, r, kindAnon, reqctx.ClientIP(r.Context())) {
			return
		}
		next(w, r)
	}
}

// idempotent wraps a merchant mutation: with an Idempotency-Key header the
// first response is stored and replayed for identical retries.
func (s *Server) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next(w, r)
			return
		}
		p, _ := auth.MerchantFrom(r.Context())
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s.writeError(w, r, bodyReadError(err))
			return
		}
		replay, err := s.idem.Begin(r.Context(), p.Merchant.ID, key, r.Method, r.URL.Path, body)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if replay != nil {
			s.metrics.IdempotentReplay.With("replayed").Inc()
			w.Header().Set("Idempotent-Replayed", "true")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(replay.Status)
			_, _ = w.Write(replay.Body)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := &statusRecorder{ResponseWriter: w, capture: true}
		completed := false
		defer func() {
			if completed {
				return
			}
			// The handler panicked; release the claim so the client can retry.
			_ = s.idem.Finish(r.Context(), p.Merchant.ID, key, http.StatusInternalServerError, nil)
		}()
		next(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		if err := s.idem.Finish(r.Context(), p.Merchant.ID, key, status, rec.body.Bytes()); err != nil {
			s.log.Error("idempotency finish failed", "request_id", reqctx.RequestID(r.Context()), "error", err)
		}
		completed = true
		s.metrics.IdempotentReplay.With("stored").Inc()
	}
}
