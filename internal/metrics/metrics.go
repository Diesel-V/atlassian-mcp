// Package metrics is a small, dependency-free Prometheus text-format exporter
// covering exactly what the service needs: labelled counters, histograms and
// gauges computed at scrape time.
package metrics

import (
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry holds all metrics and serves them over HTTP.
type Registry struct {
	mu      sync.Mutex
	metrics []writer
}

type writer interface{ write(w io.Writer) }

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) add(m writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, m)
}

// ServeHTTP writes all metrics in Prometheus text exposition format.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	r.mu.Lock()
	ms := slices.Clone(r.metrics)
	r.mu.Unlock()
	for _, m := range ms {
		m.write(w)
	}
}

// CounterVec is a counter partitioned by label values.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]float64
}

// NewCounterVec registers a counter.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labels: labels, values: map[string]float64{}}
	r.add(c)
	return c
}

// Inc adds one for the given label values (in declaration order).
func (c *CounterVec) Inc(labelValues ...string) {
	key := labelKey(c.labels, labelValues)
	c.mu.Lock()
	c.values[key]++
	c.mu.Unlock()
}

// Value returns the current value (for tests).
func (c *CounterVec) Value(labelValues ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[labelKey(c.labels, labelValues)]
}

func (c *CounterVec) write(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	for _, k := range slices.Sorted(maps.Keys(c.values)) {
		fmt.Fprintf(w, "%s%s %s\n", c.name, k, formatFloat(c.values[k]))
	}
}

// HistogramVec is a histogram partitioned by label values.
type HistogramVec struct {
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*histSeries
}

type histSeries struct {
	labelValues []string
	counts      []uint64
	sum         float64
	count       uint64
}

// DefaultDurationBuckets suit latencies of calls to Confluence/Jira (seconds).
var DefaultDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20}

// NewHistogramVec registers a histogram.
func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	h := &HistogramVec{name: name, help: help, labels: labels, buckets: slices.Clone(buckets), series: map[string]*histSeries{}}
	slices.Sort(h.buckets)
	r.add(h)
	return h
}

// Observe records a value.
func (h *HistogramVec) Observe(v float64, labelValues ...string) {
	key := labelKey(h.labels, labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.series[key]
	if !ok {
		s = &histSeries{labelValues: slices.Clone(labelValues), counts: make([]uint64, len(h.buckets))}
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

// ObserveSince records the seconds elapsed since start.
func (h *HistogramVec) ObserveSince(start time.Time, labelValues ...string) {
	h.Observe(time.Since(start).Seconds(), labelValues...)
}

func (h *HistogramVec) write(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	for _, k := range slices.Sorted(maps.Keys(h.series)) {
		s := h.series[k]
		for i, b := range h.buckets {
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelKey(append(slices.Clone(h.labels), "le"), append(slices.Clone(s.labelValues), formatFloat(b))), s.counts[i])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelKey(append(slices.Clone(h.labels), "le"), append(slices.Clone(s.labelValues), "+Inf")), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.name, k, formatFloat(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, k, s.count)
	}
}

// GaugeFunc is a gauge with one label whose values are computed on every scrape.
type GaugeFunc struct {
	name, help, label string
	collect           func() (map[string]float64, error)
	onError           func(error)
}

// NewGaugeFunc registers a gauge. collect is called on every scrape; on error the
// metric is written without samples and onError (if set) is called.
func (r *Registry) NewGaugeFunc(name, help, label string, collect func() (map[string]float64, error), onError func(error)) *GaugeFunc {
	g := &GaugeFunc{name: name, help: help, label: label, collect: collect, onError: onError}
	r.add(g)
	return g
}

func (g *GaugeFunc) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
	values, err := g.collect()
	if err != nil {
		if g.onError != nil {
			g.onError(err)
		}
		return
	}
	for _, k := range slices.Sorted(maps.Keys(values)) {
		fmt.Fprintf(w, "%s%s %s\n", g.name, labelKey([]string{g.label}, []string{k}), formatFloat(values[k]))
	}
}

func labelKey(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)

func escapeLabel(v string) string { return labelEscaper.Replace(v) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
