// Package promexporter is the one-liner bridge from an OTel MeterProvider
// into a prometheus.Registerer. Use this when the surrounding application
// scrapes via Prometheus and you still want SDK code to depend only on the
// OTel metric API.
package promexporter

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"

	"github.com/ethpandaops/agent-sdk-observability/histograms"
)

// Option tunes NewMeterProvider.
type Option func(*config)

type config struct {
	aggregation    sdkmetric.AggregationSelector
	exemplarFilter exemplar.Filter
}

func defaultConfig() config {
	return config{
		aggregation:    histograms.AggregationSelector,
		exemplarFilter: exemplar.TraceBasedFilter,
	}
}

// WithAggregationSelector overrides the default exponential-histogram selector.
func WithAggregationSelector(s sdkmetric.AggregationSelector) Option {
	return func(c *config) { c.aggregation = s }
}

// WithExemplarFilter overrides the default trace-based exemplar filter.
func WithExemplarFilter(f exemplar.Filter) Option {
	return func(c *config) { c.exemplarFilter = f }
}

// NewMeterProvider returns a MeterProvider whose metrics are registered with
// the given prometheus.Registerer. Exponential histograms and trace-based
// exemplars are enabled by default.
func NewMeterProvider(reg prometheus.Registerer, opts ...Option) (metric.MeterProvider, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	exporter, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		otelprom.WithAggregationSelector(cfg.aggregation),
	)
	if err != nil {
		return nil, fmt.Errorf("build prometheus exporter: %w", err)
	}

	return sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithExemplarFilter(cfg.exemplarFilter),
	), nil
}
