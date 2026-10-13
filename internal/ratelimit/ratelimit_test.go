package ratelimit

import (
	"testing"
	"time"
)

type fakeTime struct{ t time.Time }

func (f *fakeTime) now() time.Time { return f.t }

func TestBucketAllowsBurstThenRefills(t *testing.T) {
	ft := &fakeTime{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := New(2, 3, ft.now) // 2 tokens/s, burst 3
	for i := 0; i < 3; i++ {
		if d := l.Allow("k"); !d.Allowed || d.Remaining != 2-i || d.Limit != 3 {
			t.Fatalf("request %d: %+v", i, d)
		}
	}
	d := l.Allow("k")
	if d.Allowed || d.RetryAfter != 500*time.Millisecond {
		t.Fatalf("over burst: %+v", d)
	}
	ft.t = ft.t.Add(500 * time.Millisecond)
	if d := l.Allow("k"); !d.Allowed {
		t.Fatalf("after refill: %+v", d)
	}
	// Keys are independent.
	if d := l.Allow("other"); !d.Allowed || d.Remaining != 2 {
		t.Fatalf("other key: %+v", d)
	}
	// Refill never exceeds the burst.
	ft.t = ft.t.Add(time.Hour)
	if d := l.Allow("k"); d.Remaining != 2 {
		t.Fatalf("capped refill: %+v", d)
	}
}

func TestSweepAndZeroRate(t *testing.T) {
	ft := &fakeTime{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := New(1, 1, ft.now)
	l.Allow("a")
	l.Allow("b")
	if n := l.Sweep(); n != 2 {
		t.Fatalf("drained buckets must survive the sweep: %d", n)
	}
	ft.t = ft.t.Add(2 * time.Second)
	if n := l.Sweep(); n != 0 {
		t.Fatalf("refilled buckets should be evicted: %d", n)
	}
	z := New(0, 1, ft.now)
	z.Allow("x")
	if d := z.Allow("x"); d.Allowed || d.RetryAfter != time.Hour {
		t.Fatalf("zero rate: %+v", d)
	}
	if New(1, 0, nil).Allow("y").Limit != 1 {
		t.Fatal("burst must be at least one")
	}
}
