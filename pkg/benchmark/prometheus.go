// Package benchmark — Prometheus integration.
//
// PrometheusExporter implements the metric contract described in
// docs/observability/README.md. The Grafana dashboard
// (docs/observability/grafana/iterion-workflow.json) queries the metric
// names defined here, so any rename must update both ends in lock-step.
package benchmark

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
)

// PrometheusExporter wires iterion's EventHooks and event emitter to
// Prometheus counters, histograms, and gauges. Construct one per run
// (so run_id labels stay attached) and chain it onto the existing
// store-backed observers via model.ChainHooks and (*PrometheusExporter).
// WrapEmitter.
type PrometheusExporter struct {
	registry *prometheus.Registry
	runID    string

	nodeCostUSD      *prometheus.CounterVec
	nodeTokens       *prometheus.CounterVec
	llmRequests      *prometheus.CounterVec
	llmRetries       *prometheus.CounterVec
	toolCalls        *prometheus.CounterVec
	nodeDurationMs   *prometheus.HistogramVec
	parallelGauge    prometheus.Gauge
	branchesInFlight int64
}

// NewPrometheusExporter constructs an exporter and registers all
// metrics on the given registry. Pass `nil` to use a fresh
// prometheus.NewRegistry() (the default for /metrics endpoints that
// should not leak the global registry's other collectors).
func NewPrometheusExporter(runID string, registry *prometheus.Registry) *PrometheusExporter {
	if registry == nil {
		registry = prometheus.NewRegistry()
	}
	pe := &PrometheusExporter{
		registry: registry,
		runID:    runID,
		nodeCostUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "iterion_node_cost_usd_total",
			Help: "Cumulative LLM cost (USD) attributed to each iterion node.",
		}, []string{"node_id", "run_id"}),
		nodeTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "iterion_node_tokens_total",
			Help: "Cumulative LLM tokens (input + output) consumed by each iterion node.",
		}, []string{"node_id", "run_id", "model"}),
		llmRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "iterion_llm_request_total",
			Help: "Number of LLM requests issued, partitioned by node and model.",
		}, []string{"node_id", "model"}),
		llmRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "iterion_llm_retry_total",
			Help: "Number of LLM retries, partitioned by node and model.",
		}, []string{"node_id", "model"}),
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "iterion_tool_call_total",
			Help: "Number of tool calls executed inside an LLM tool loop.",
		}, []string{"node_id", "tool"}),
		nodeDurationMs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "iterion_node_duration_ms",
			Help:    "Per-node execution duration in milliseconds.",
			Buckets: []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 300000},
		}, []string{"node_id"}),
		parallelGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "iterion_parallel_branches",
			Help: "Number of fan-out branches currently in flight for the run.",
		}),
	}

	registry.MustRegister(
		pe.nodeCostUSD,
		pe.nodeTokens,
		pe.llmRequests,
		pe.llmRetries,
		pe.toolCalls,
		pe.nodeDurationMs,
		pe.parallelGauge,
	)
	return pe
}

// Registry exposes the prometheus.Registry so callers can layer extra
// collectors (or use a custom HTTP handler).
func (p *PrometheusExporter) Registry() *prometheus.Registry { return p.registry }

// Handler returns an http.Handler serving the Prometheus text-exposition
// format for this exporter's metrics. Mount it at /metrics.
func (p *PrometheusExporter) Handler() http.Handler {
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{Registry: p.registry})
}

// EventHooks returns the model.EventHooks that drive the per-node
// counters (cost, tokens, requests, retries, tool calls, duration).
func (p *PrometheusExporter) EventHooks() model.EventHooks {
	return model.EventHooks{
		OnLLMRequest: func(nodeID string, info model.LLMRequestInfo) {
			p.llmRequests.WithLabelValues(nodeID, info.Model).Inc()
		},
		OnLLMRetry: func(nodeID string, info model.RetryInfo) {
			// RetryInfo doesn't carry the model; use empty label.
			p.llmRetries.WithLabelValues(nodeID, "").Inc()
		},
		OnLLMResponse: func(nodeID string, info model.LLMResponseInfo) {
			if info.Latency > 0 {
				p.nodeDurationMs.WithLabelValues(nodeID).Observe(float64(info.Latency.Milliseconds()))
			}
		},
		OnToolCall: func(nodeID string, info model.LLMToolCallInfo) {
			p.toolCalls.WithLabelValues(nodeID, info.ToolName).Inc()
		},
		OnNodeFinished: func(nodeID string, output map[string]interface{}) {
			model := stringField(output, "_model")
			if t, ok := numericField(output, "_tokens"); ok && t > 0 {
				p.nodeTokens.WithLabelValues(nodeID, p.runID, model).Add(t)
			}
			if c, ok := numericField(output, "_cost_usd"); ok && c > 0 {
				p.nodeCostUSD.WithLabelValues(nodeID, p.runID).Add(c)
			}
		},
	}
}

// EventObserver returns a callback compatible with
// runtime.WithEventObserver. It updates the parallel-branches gauge
// based on branch_started / branch_finished events.
//
// The callback runs synchronously on the engine's emit path, so we
// recover from any panic in the metrics layer to avoid taking down a
// running workflow because of an observability hiccup.
func (p *PrometheusExporter) EventObserver() func(store.Event) {
	return func(evt store.Event) {
		defer func() {
			if r := recover(); r != nil {
				// Best-effort: we deliberately drop the event rather than
				// propagating the panic into the engine loop.
				_ = r
			}
		}()
		switch evt.Type {
		case store.EventBranchStarted:
			v := atomic.AddInt64(&p.branchesInFlight, 1)
			p.parallelGauge.Set(float64(v))
		case store.EventBranchFinished:
			v := atomic.AddInt64(&p.branchesInFlight, -1)
			if v < 0 {
				// Defensive: clamp to 0 if a branch finishes more than once.
				atomic.StoreInt64(&p.branchesInFlight, 0)
				v = 0
			}
			p.parallelGauge.Set(float64(v))
		}
	}
}

// stringField extracts a string-typed value from a map, returning "" on
// absence or type mismatch.
func stringField(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// numericField extracts a numeric value (int or float64) and reports
// whether the conversion succeeded. The bool is false when the key is
// missing or carries an unsupported type.
func numericField(m map[string]interface{}, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case time.Duration:
		return float64(t), true
	}
	return 0, false
}
