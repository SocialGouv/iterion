package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/metric"

	"github.com/ethpandaops/agent-sdk-observability/promexporter"
)

// NewPrometheusMeterProvider returns an OTel MeterProvider backed by the given
// Prometheus registerer, with exponential histograms and trace-based exemplars
// applied by the shared observability helpers.
func NewPrometheusMeterProvider(reg prometheus.Registerer) (metric.MeterProvider, error) {
	return promexporter.NewMeterProvider(reg)
}
