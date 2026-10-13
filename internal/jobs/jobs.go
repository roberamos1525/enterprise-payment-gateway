// Package jobs runs periodic background tasks (authorization expiry,
// idempotency purge, payouts, scheduled settlement) and lets operators
// trigger them on demand.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/robertramos/enterprise-payment-gateway/internal/metrics"
)

// Job is a named periodic task.
type Job struct {
	Name     string
	Interval time.Duration // <= 0: manual only
	Run      func(ctx context.Context) (any, error)
}

// Runner schedules jobs. Each job runs on its own goroutine; a job never
// overlaps with itself (manual triggers wait for a scheduled run to finish).
type Runner struct {
	log     *slog.Logger
	metrics *metrics.Gateway
	mu      sync.Mutex
	jobs    map[string]*entry
	wg      sync.WaitGroup
}

type entry struct {
	Job
	running sync.Mutex
}

// NewRunner builds a runner.
func NewRunner(log *slog.Logger, m *metrics.Gateway) *Runner {
	return &Runner{log: log, metrics: m, jobs: map[string]*entry{}}
}

// Register adds a job; names must be unique.
func (r *Runner) Register(j Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.jobs[j.Name]; dup {
		panic("jobs: duplicate job " + j.Name)
	}
	r.jobs[j.Name] = &entry{Job: j}
}

// Names lists registered jobs.
func (r *Runner) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.jobs))
	for n := range r.jobs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RunNow executes a job immediately and returns its result.
func (r *Runner) RunNow(ctx context.Context, name string) (any, error) {
	r.mu.Lock()
	e, ok := r.jobs[name]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("jobs: unknown job %q", name)
	}
	return r.execute(ctx, e)
}

func (r *Runner) execute(ctx context.Context, e *entry) (res any, err error) {
	e.running.Lock()
	defer e.running.Unlock()
	start := time.Now()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("jobs: %s panicked: %v", e.Name, p)
		}
		result := "ok"
		if err != nil {
			result = "error"
			r.log.Error("job failed", "job", e.Name, "error", err, "duration", time.Since(start))
		} else {
			r.log.Debug("job finished", "job", e.Name, "result", res, "duration", time.Since(start))
		}
		if r.metrics != nil {
			r.metrics.JobRuns.With(e.Name, result).Inc()
		}
	}()
	return e.Run(ctx)
}

// Start launches the periodic loops; they stop when ctx is cancelled.
func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.jobs {
		if e.Interval <= 0 {
			continue
		}
		r.wg.Add(1)
		go func(e *entry) {
			defer r.wg.Done()
			t := time.NewTicker(e.Interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					_, _ = r.execute(ctx, e)
				}
			}
		}(e)
	}
}

// Wait blocks until all loops started by Start have exited.
func (r *Runner) Wait() { r.wg.Wait() }
