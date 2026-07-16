package observability

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
	upstreamgenai "go.opentelemetry.io/otel/semconv/v1.40.0/genaiconv"
)

func (o *Observer) initMetrics(meter metric.Meter) error {
	var err error

	// Spec GenAI metrics — upstream instrument structs.
	if o.opDuration, err = upstreamgenai.NewClientOperationDuration(meter); err != nil {
		return fmt.Errorf("gen_ai.client.operation.duration: %w", err)
	}

	if o.tokenUsage, err = upstreamgenai.NewClientTokenUsage(meter); err != nil {
		return fmt.Errorf("gen_ai.client.token.usage: %w", err)
	}

	// TTFT — spec metric (Development status). The upstream genaiconv package
	// at v1.40.0 only ships `gen_ai.server.time_to_first_token`; the
	// client-side variant is defined in the spec but not yet exposed as an
	// instrument struct. Construct it via raw meter so we emit the
	// spec-conformant name today and can switch to the upstream struct when
	// it lands.
	if o.ttft, err = meter.Float64Histogram(
		"gen_ai.client.operation.time_to_first_chunk",
		metric.WithDescription("Time to receive the first chunk in a streaming GenAI operation"),
		metric.WithUnit("s"),
	); err != nil {
		return fmt.Errorf("gen_ai.client.operation.time_to_first_chunk: %w", err)
	}

	// SDK-specific duration histograms.
	if o.toolCallDuration, err = meter.Float64Histogram(
		"codex.tool_call_duration",
		metric.WithDescription("Duration of tool call executions"),
		metric.WithUnit("s"),
	); err != nil {
		return fmt.Errorf("codex.tool_call_duration: %w", err)
	}

	if o.hookDispatchDuration, err = meter.Float64Histogram(
		"codex.hook_dispatch_duration",
		metric.WithDescription("Duration of hook dispatches"),
		metric.WithUnit("s"),
	); err != nil {
		return fmt.Errorf("codex.hook_dispatch_duration: %w", err)
	}

	// Counters — `_total` suffix preserved by the Prometheus exporter.
	if o.toolCallsTotal, err = meter.Int64Counter(
		"codex.tool_calls_total",
		metric.WithDescription("Total number of tool call invocations"),
		metric.WithUnit("{call}"),
	); err != nil {
		return fmt.Errorf("codex.tool_calls_total: %w", err)
	}

	// Cost accumulator in USD.
	if o.costTotal, err = meter.Float64Counter(
		"gen_ai.client.cost_usd_total",
		metric.WithDescription("Total cost in USD incurred by the GenAI client"),
		metric.WithUnit("{USD}"),
	); err != nil {
		return fmt.Errorf("gen_ai.client.cost_usd_total: %w", err)
	}

	return nil
}
