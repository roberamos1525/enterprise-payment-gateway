package jobs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/logging"
	"github.com/robertramos/enterprise-payment-gateway/internal/metrics"
)

func TestRunNowAndErrors(t *testing.T) {
	m := metrics.NewGateway()
	r := NewRunner(logging.Discard(), m)
	r.Register(Job{Name: "ok", Run: func(context.Context) (any, error) { return 42, nil }})
	r.Register(Job{Name: "fail", Run: func(context.Context) (any, error) { return nil, errors.New("nope") }})
	r.Register(Job{Name: "panic", Run: func(context.Context) (any, error) { panic("boom") }})
	if got := r.Names(); len(got) != 3 || got[0] != "fail" {
		t.Fatalf("names = %v", got)
	}
	if v, err := r.RunNow(context.Background(), "ok"); err != nil || v != 42 {
		t.Fatalf("ok = %v %v", v, err)
	}
	if _, err := r.RunNow(context.Background(), "fail"); err == nil {
		t.Fatal("error swallowed")
	}
	if _, err := r.RunNow(context.Background(), "panic"); err == nil {
		t.Fatal("panic not converted to error")
	}
	if _, err := r.RunNow(context.Background(), "missing"); err == nil {
		t.Fatal("unknown job accepted")
	}
	if m.JobRuns.With("fail", "error").Value() != 1 || m.JobRuns.With("ok", "ok").Value() != 1 {
		t.Fatal("job metrics not recorded")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration should panic")
		}
	}()
	r.Register(Job{Name: "ok"})
}

func TestPeriodicLoopStopsOnCancel(t *testing.T) {
	r := NewRunner(logging.Discard(), nil)
	var n atomic.Int32
	r.Register(Job{Name: "tick", Interval: 5 * time.Millisecond, Run: func(context.Context) (any, error) {
		n.Add(1)
		return nil, nil
	}})
	r.Register(Job{Name: "manual", Run: func(context.Context) (any, error) {
		t.Error("manual-only job must not be scheduled")
		return nil, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	r.Wait()
	if n.Load() < 3 {
		t.Fatalf("job ran %d times", n.Load())
	}
}
