package metrics

import (
	"maps"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// GaugeSnapshot publishes a complete sample for one label dimension. Published
// vectors are immutable: a concurrent Collect retains its original vector until
// every metric has been read, even if a replacement retires or changes labels.
// Constructed by the registry; callers publish only complete successful reads.
type GaugeSnapshot struct {
	opts    prometheus.GaugeOpts
	label   string
	current atomic.Pointer[prometheus.GaugeVec]
}

func newGaugeSnapshot(opts prometheus.GaugeOpts, label string) *GaugeSnapshot {
	opts.ConstLabels = maps.Clone(opts.ConstLabels)
	g := &GaugeSnapshot{opts: opts, label: label}
	g.Replace(nil)
	return g
}

// Replace copies values into an unpublished vector and swaps it atomically.
// Callers may reuse their map after this call; an empty map retires all labels.
// Individual gauge handles are never exposed, so published samples cannot be
// mutated while Prometheus writes their values after Collect returns.
func (g *GaugeSnapshot) Replace(values map[string]float64) {
	next := prometheus.NewGaugeVec(g.opts, []string{g.label})
	for label, value := range values {
		next.WithLabelValues(label).Set(value)
	}
	g.current.Store(next)
}

func (g *GaugeSnapshot) Describe(ch chan<- *prometheus.Desc) {
	g.current.Load().Describe(ch)
}

func (g *GaugeSnapshot) Collect(ch chan<- prometheus.Metric) {
	g.current.Load().Collect(ch)
}
