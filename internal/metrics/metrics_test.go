package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestExposition(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("demo_total", "A demo counter.", "route", "status")
	c.With("/a", "2xx").Inc()
	c.With("/a", "2xx").Add(2)
	c.With("/b \"q\"", "5xx").Inc()
	c.With("/a", "2xx").Add(-5) // ignored: counters never decrease
	h := r.NewHistogramVec("demo_seconds", "A demo histogram.", []float64{0.1, 1}, "op")
	h.Observe(0.05, "x")
	h.Observe(0.5, "x")
	h.Observe(5, "x")
	r.NewGaugeFunc("demo_gauge", "A demo gauge.", "state", func() map[string]float64 { return map[string]float64{"on": 2} })
	r.NewGaugeFunc("demo_plain", "Unlabelled.", "", func() map[string]float64 { return map[string]float64{"": 7} })

	var b strings.Builder
	r.Write(&b)
	out := b.String()
	for _, want := range []string{
		"# TYPE demo_total counter",
		`demo_total{route="/a",status="2xx"} 3`,
		`demo_total{route="/b \"q\"",status="5xx"} 1`,
		"# TYPE demo_seconds histogram",
		`demo_seconds_bucket{op="x",le="0.1"} 1`,
		`demo_seconds_bucket{op="x",le="1"} 2`,
		`demo_seconds_bucket{op="x",le="+Inf"} 3`,
		`demo_seconds_sum{op="x"} 5.55`,
		`demo_seconds_count{op="x"} 3`,
		`demo_gauge{state="on"} 2`,
		"demo_plain 7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "demo_gauge") > strings.Index(out, "demo_seconds") {
		t.Fatal("families must be sorted by name")
	}
	if c.With("/a", "2xx").Value() != 3 {
		t.Fatal("Value() wrong")
	}
}

func TestConcurrentCountsAndPanics(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("c_total", "c", "k")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.With("x").Inc()
			}
		}()
	}
	wg.Wait()
	if c.With("x").Value() != 2000 {
		t.Fatalf("lost updates: %v", c.With("x").Value())
	}
	mustPanic(t, func() { c.With("a", "b") })
	mustPanic(t, func() { r.NewCounterVec("c_total", "dup") })
	g := NewGateway()
	g.PaymentOps.With("capture", "captured").Inc()
	var b strings.Builder
	g.Registry.Write(&b)
	if !strings.Contains(b.String(), `gateway_payment_operations_total{action="capture",status="captured"} 1`) {
		t.Fatal("gateway metrics not registered")
	}
}

func mustPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	f()
}
