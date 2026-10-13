// Package clock abstracts wall-clock time so time-dependent logic
// (authorization expiry, settlement days, webhook backoff) is testable.
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time.
type Clock interface {
	Now() time.Time
}

// Real is the system clock, always in UTC.
type Real struct{}

// Now implements Clock.
func (Real) Now() time.Time { return time.Now().UTC() }

// Fake is a manually-controlled clock for tests.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a fake clock set to t.
func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

// Now implements Clock.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

// Set moves the clock to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	f.now = t.UTC()
	f.mu.Unlock()
}

// StartOfDay truncates t to midnight UTC.
func StartOfDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// DateLayout is the canonical YYYY-MM-DD layout used for settlement days.
const DateLayout = "2006-01-02"

// ParseDate parses a YYYY-MM-DD date as midnight UTC.
func ParseDate(s string) (time.Time, error) {
	return time.ParseInLocation(DateLayout, s, time.UTC)
}
