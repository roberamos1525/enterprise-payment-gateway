// Package metrics is a small, dependency-free metrics registry that renders
// the Prometheus text exposition format (version 0.0.4).
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds metric families.
type Registry struct {
	mu       sync.Mutex
	families map[string]family
}

type family interface {
	write(w io.Writer)
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{families: map[string]family{}} }

func (r *Registry) register(name string, f family) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.families[name]; dup {
		panic("metrics: duplicate metric " + name)
	}
	r.families[name] = f
}

// Write renders all metrics in name order.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	fams := make([]family, 0, len(names))
	sort.Strings(names)
	for _, n := range names {
		fams = append(fams, r.families[n])
	}
	r.mu.Unlock()
	for _, f := range fams {
		f.write(w)
	}
}

// labelKey joins label values with a separator that cannot appear in them
// after escaping.
func labelKey(values []string) string { return strings.Join(values, "\xff") }

func formatLabels(names, values []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		parts = append(parts, n+`="`+escape(values[i])+`"`)
	}
	parts = append(parts, extra...)
	return "{" + strings.Join(parts, ",") + "}"
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}

func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// --- counters ---------------------------------------------------------------

// CounterVec is a monotonically increasing counter partitioned by labels.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.RWMutex
	series     map[string]*counterSeries
}

type counterSeries struct {
	values []string
	v      atomic.Uint64 // float64 bits
}

// NewCounterVec registers a counter family.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labels: labels, series: map[string]*counterSeries{}}
	r.register(name, c)
	return c
}

// Counter is a handle to one labelled series.
type Counter struct{ s *counterSeries }

// With returns the series for the label values (created on first use).
func (c *CounterVec) With(values ...string) Counter {
	if len(values) != len(c.labels) {
		panic(fmt.Sprintf("metrics: %s expects %d labels, got %d", c.name, len(c.labels), len(values)))
	}
	key := labelKey(values)
	c.mu.RLock()
	s, ok := c.series[key]
	c.mu.RUnlock()
	if ok {
		return Counter{s}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok = c.series[key]; !ok {
		s = &counterSeries{values: append([]string(nil), values...)}
		c.series[key] = s
	}
	return Counter{s}
}

// Inc adds one.
func (c Counter) Inc() { c.Add(1) }

// Add adds a non-negative delta.
func (c Counter) Add(d float64) {
	if d < 0 {
		return
	}
	for {
		old := c.s.v.Load()
		if c.s.v.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+d)) {
			return
		}
	}
}

// Value returns the current value.
func (c Counter) Value() float64 { return math.Float64frombits(c.s.v.Load()) }

func (c *CounterVec) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, k := range sortedKeys(c.series) {
		s := c.series[k]
		fmt.Fprintf(w, "%s%s %s\n", c.name, formatLabels(c.labels, s.values),
			formatFloat(math.Float64frombits(s.v.Load())))
	}
}

// --- gauges -------------------------------------------------------------------

// GaugeFunc is a gauge whose labelled values are computed at scrape time.
type GaugeFunc struct {
	name, help string
	labels     []string
	fn         func() map[string]float64 // key = single label value (or "" when unlabelled)
}

// NewGaugeFunc registers a gauge evaluated on every scrape. With one label,
// fn returns label value -> gauge value; with no labels, use key "".
func (r *Registry) NewGaugeFunc(name, help string, label string, fn func() map[string]float64) {
	g := &GaugeFunc{name: name, help: help, fn: fn}
	if label != "" {
		g.labels = []string{label}
	}
	r.register(name, g)
}

func (g *GaugeFunc) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
	vals := g.fn()
	for _, k := range sortedKeys(vals) {
		if len(g.labels) == 0 {
			fmt.Fprintf(w, "%s %s\n", g.name, formatFloat(vals[k]))
			continue
		}
		fmt.Fprintf(w, "%s%s %s\n", g.name, formatLabels(g.labels, []string{k}), formatFloat(vals[k]))
	}
}

// --- histograms -------------------------------------------------------------

// HistogramVec observes value distributions in cumulative buckets.
type HistogramVec struct {
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*histSeries
}

type histSeries struct {
	values []string
	counts []uint64
	sum    float64
	count  uint64
}

// DefBuckets are latency buckets in seconds.
var DefBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// NewHistogramVec registers a histogram family.
func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	h := &HistogramVec{name: name, help: help, labels: labels, buckets: b, series: map[string]*histSeries{}}
	r.register(name, h)
	return h
}

// Observe records a value for the label values.
func (h *HistogramVec) Observe(v float64, values ...string) {
	if len(values) != len(h.labels) {
		panic(fmt.Sprintf("metrics: %s expects %d labels, got %d", h.name, len(h.labels), len(values)))
	}
	key := labelKey(values)
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[key]
	if !ok {
		s = &histSeries{values: append([]string(nil), values...), counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.sum += v
	s.count++
}

func (h *HistogramVec) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range sortedKeys(h.series) {
		s := h.series[k]
		for i, b := range h.buckets {
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, formatLabels(h.labels, s.values, `le="`+formatFloat(b)+`"`), s.counts[i])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, formatLabels(h.labels, s.values, `le="+Inf"`), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.name, formatLabels(h.labels, s.values), formatFloat(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, formatLabels(h.labels, s.values), s.count)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
