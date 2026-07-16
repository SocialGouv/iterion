// Package prometheus provides a convenience helper for wiring Prometheus metrics
// into the Codex Agent SDK without pulling OTel SDK dependencies into the
// root module for every consumer.
//
// Usage:
//
//	mp, err := prometheus.NewMeterProvider(reg)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	codexsdk.WithMeterProvider(mp)
//
// The returned MeterProvider is built via the shared
// agent-sdk-observability/promexporter helper, which applies exponential
// histograms and trace-based exemplars uniformly across all ethPandaOps agent
// SDKs.
package prometheus

import (
	prom "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/metric"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/observability"
)

// NewMeterProvider creates an OTel MeterProvider backed by the given Prometheus
// registerer. The returned provider can be passed to codexsdk.WithMeterProvider.
func NewMeterProvider(reg prom.Registerer) (metric.MeterProvider, error) {
	return observability.NewPrometheusMeterProvider(reg)
}
