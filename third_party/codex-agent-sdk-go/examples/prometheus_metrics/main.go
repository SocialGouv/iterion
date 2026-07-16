// Package main demonstrates using WithPrometheusRegisterer for OpenTelemetry
// observability with the Codex Agent SDK.
//
// WithPrometheusRegisterer is the simplest way to get Prometheus metrics from
// the SDK. It automatically creates an OTel MeterProvider backed by the
// provided Prometheus registerer via the OTel→Prometheus bridge.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func main() {
	fmt.Println("=== Prometheus Metrics Example ===")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create a Prometheus registry. In production, use prometheus.DefaultRegisterer
	// or a custom registry exposed via an HTTP handler for scraping.
	reg := prometheus.NewRegistry()

	// Create an OTel TracerProvider for distributed tracing.
	tp := sdktrace.NewTracerProvider()

	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			logger.Error("failed to shutdown tracer provider", "error", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Pass the Prometheus registerer and tracer provider to the SDK.
	// When WithPrometheusRegisterer is set (and WithMeterProvider is not),
	// the SDK automatically creates an OTel MeterProvider from the registerer.
	//
	// The SDK records (via OpenTelemetry):
	//   - gen_ai.client.operation.duration (histogram, GenAI semconv)
	//   - gen_ai.client.token.usage (histogram, GenAI semconv)
	//   - gen_ai.client.operation.time_to_first_chunk (histogram, TTFT)
	//   - gen_ai.client.cost_usd_total (counter, USD cost)
	//   - codex.tool_calls_total (counter, per tool name + outcome)
	//   - codex.tool_call_duration (histogram, per tool)
	//   - codex.hook_dispatch_duration (histogram, per hook event)
	//   - One span per Query/QueryStream call (GenAI "chat" span)
	//   - Child spans per tool invocation (GenAI "execute_tool" span)
	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("What is 2 + 2?"),
		codexsdk.WithLogger(logger),
		codexsdk.WithPrometheusRegisterer(reg),
		codexsdk.WithTracerProvider(tp),
		codexsdk.WithPermissionMode("bypassPermissions"),
	) {
		if err != nil {
			fmt.Printf("Error: %v\n", err)

			return
		}

		switch m := msg.(type) {
		case *codexsdk.AssistantMessage:
			for _, block := range m.Content {
				if textBlock, ok := block.(*codexsdk.TextBlock); ok {
					fmt.Printf("Codex: %s\n", textBlock.Text)
				}
			}

		case *codexsdk.ResultMessage:
			fmt.Println("Query complete")

			if m.Usage != nil {
				fmt.Printf("Tokens: %d in / %d out\n",
					m.Usage.InputTokens, m.Usage.OutputTokens)
			}
		}
	}

	// In production, expose reg via promhttp.HandlerFor(reg, ...) on an
	// HTTP endpoint for Prometheus to scrape.
	families, err := reg.Gather()
	if err != nil {
		logger.Error("failed to gather metrics", "error", err)

		return
	}

	fmt.Printf("\nCollected %d metric families from Prometheus registry\n", len(families))

	for _, fam := range families {
		fmt.Printf("  %s\n", fam.GetName())
	}
}
