// Package tracing wires the OpenTelemetry trace SDK so the spans
// emitted by pkg/runtime, pkg/runner, and pkg/server are actually
// exported. Without an explicit TracerProvider the global tracer
// drops every span — instrumentation in the rest of the tree relies
// on this package being initialised at boot in cloud-mode binaries.
//
// The exporter is OTLP/HTTP (protobuf) because it is the lightest
// transport that every OTel-compatible collector understands. Endpoint
// + headers + sampling come from the standard `OTEL_EXPORTER_OTLP_*`
// env vars so operators get the same configuration surface they use
// for any other Go service.
//
// Plan §F (T-41) — bridge from instrumentation to actual export.
package tracing

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Init configures the global TracerProvider when an OTLP endpoint is
// set, otherwise it returns a no-op shutdown so callers can defer
// unconditionally. The env vars consulted are the standard OTel ones:
//
//   - OTEL_EXPORTER_OTLP_ENDPOINT (or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT)
//   - OTEL_EXPORTER_OTLP_HEADERS
//   - OTEL_EXPORTER_OTLP_PROTOCOL (only "http/protobuf" supported here)
//
// serviceName populates the `service.name` resource attribute. The
// returned shutdown flushes pending spans up to its context deadline.
func Init(ctx context.Context, serviceName string, logger *iterlog.Logger) (func(context.Context) error, error) {
	endpoint := firstNonEmpty(
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	)
	if endpoint == "" {
		// No exporter configured — install the W3C propagator so
		// inbound trace context is still respected, but skip the
		// SDK setup entirely. Spans created by the rest of the tree
		// are no-ops.
		otel.SetTextMapPropagator(propagation.TraceContext{})
		if logger != nil {
			logger.Info("tracing: OTEL_EXPORTER_OTLP_ENDPOINT not set — spans will be dropped (propagator only)")
		}
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptrace.New(ctx, otlptracehttp.NewClient())
	if err != nil {
		return nil, fmt.Errorf("tracing: build OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: build resource: %w", err)
	}

	tp := tracesdk.NewTracerProvider(
		tracesdk.WithBatcher(exp),
		tracesdk.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	if logger != nil {
		logger.Info("tracing: OTLP/HTTP exporter wired (endpoint=%s, service=%s)", endpoint, serviceName)
	}
	return tp.Shutdown, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
