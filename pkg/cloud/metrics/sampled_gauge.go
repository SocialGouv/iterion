package metrics

import "github.com/prometheus/client_golang/prometheus"

// SampledGauge is a gauge that has no series until its sampler first sets
// it. Every process builds the same registry — the runner serves it on its
// metrics port beside the server — so a plain gauge a server-only sampler
// owns would be published at its registered zero by every process that never
// runs the sampler, and a reader would take that zero for a reading nobody
// took. Only a process that samples publishes the series.
type SampledGauge struct {
	vec *prometheus.GaugeVec
}

func newSampledGauge(opts prometheus.GaugeOpts) *SampledGauge {
	return &SampledGauge{vec: prometheus.NewGaugeVec(opts, nil)}
}

// Set publishes v, creating the series on the first call.
func (g *SampledGauge) Set(v float64) { g.vec.WithLabelValues().Set(v) }

func (g *SampledGauge) Describe(ch chan<- *prometheus.Desc) { g.vec.Describe(ch) }

func (g *SampledGauge) Collect(ch chan<- prometheus.Metric) { g.vec.Collect(ch) }
