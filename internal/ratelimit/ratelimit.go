// Package ratelimit implements per-key token buckets.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter holds one token bucket per key. Buckets refill continuously at
// Rate tokens per second up to Burst. Idle buckets are evicted by Sweep.
type Limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	now     func() time.Time
	buckets map[string]*bucket
	maxKeys int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New builds a limiter. now may be nil (time.Now).
func New(ratePerSecond float64, burst int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	if burst < 1 {
		burst = 1
	}
	return &Limiter{rate: ratePerSecond, burst: float64(burst), now: now, buckets: map[string]*bucket{}, maxKeys: 100_000}
}

// Decision is the outcome of a rate-limit check.
type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration
	Reset      time.Duration // time until the bucket is full again
}

// Allow consumes one token for key if available.
func (l *Limiter) Allow(key string) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			l.sweepLocked(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
	}
	b.last = now
	d := Decision{Limit: int(l.burst)}
	if b.tokens >= 1 {
		b.tokens--
		d.Allowed = true
	} else if l.rate > 0 {
		d.RetryAfter = time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	} else {
		d.RetryAfter = time.Hour
	}
	d.Remaining = int(math.Floor(b.tokens))
	if l.rate > 0 {
		d.Reset = time.Duration((l.burst - b.tokens) / l.rate * float64(time.Second))
	}
	return d
}

// Sweep evicts buckets that have refilled completely (they are
// indistinguishable from new ones). It returns the remaining key count.
func (l *Limiter) Sweep() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(l.now())
	return len(l.buckets)
}

func (l *Limiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}
