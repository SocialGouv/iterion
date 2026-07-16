package codexsdk

import (
	"context"
	"testing"

	"github.com/ethpandaops/agent-sdk-observability/testkit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/message"
)

const testModel = "codex-model"

const (
	metricTokenUsage        = "gen_ai.client.token.usage"
	metricCostTotal         = "gen_ai.client.cost_usd_total"
	metricOperationDuration = "gen_ai.client.operation.duration"
	metricToolCallsTotal    = "codex.tool_calls_total"
	metricToolCallDuration  = "codex.tool_call_duration"
	metricTTFT              = "gen_ai.client.operation.time_to_first_chunk"
)

func TestNewOTelRecorder_NilProviders(t *testing.T) {
	t.Parallel()

	recorder := newOTelRecorder(nil, nil)
	assert.Nil(t, recorder, "recorder should be nil when both providers are nil")
}

func TestNewOTelRecorder_WithMeterProvider(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	recorder := newOTelRecorder(metrics.Provider(), nil)
	require.NotNil(t, recorder, "recorder should not be nil with meter provider")
	require.NotNil(t, recorder.obs, "observer should be initialized")
}

func TestNewOTelRecorder_WithTracerProvider(t *testing.T) {
	t.Parallel()

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	recorder := newOTelRecorder(nil, traces.Provider())
	require.NotNil(t, recorder, "recorder should not be nil with tracer provider")
	require.NotNil(t, recorder.obs, "observer should be initialized")
}

func TestOTelRecorder_ObserveNilSafe(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	var recorder *otelRecorder

	recorder.Observe(ctx, nil)
	recorder.Observe(ctx, &message.ResultMessage{})
}

func TestOTelRecorder_ObserveResultMessage(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	recorder := newOTelRecorder(metrics.Provider(), nil)
	require.NotNil(t, recorder)

	cost := 0.0025

	result := &message.ResultMessage{
		DurationMs:   1500,
		NumTurns:     3,
		TotalCostUSD: &cost,
		Usage: &message.Usage{
			InputTokens:           100,
			OutputTokens:          50,
			CachedInputTokens:     20,
			ReasoningOutputTokens: 10,
		},
	}

	// Cache the model first via an assistant message.
	recorder.Observe(context.Background(), &message.AssistantMessage{Model: testModel})
	recorder.Observe(context.Background(), result)

	names := metricNames(t, metrics)
	assert.Contains(t, names, metricTokenUsage)
	assert.Contains(t, names, metricCostTotal)
	assert.Contains(t, names, metricOperationDuration)
}

func TestOTelRecorder_ObserveResultWithError(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	recorder := newOTelRecorder(metrics.Provider(), nil)
	require.NotNil(t, recorder)

	stopReason := "rate limit exceeded"

	result := &message.ResultMessage{
		DurationMs: 500,
		IsError:    true,
		StopReason: &stopReason,
		Usage:      &message.Usage{InputTokens: 50},
	}

	recorder.Observe(context.Background(), result)

	points, err := metrics.HistogramPoints(context.Background(), metricOperationDuration)
	require.NoError(t, err)
	require.NotEmpty(t, points, "duration histogram should have a point")

	var sawRateLimited bool

	for _, p := range points {
		if p.Attributes["error.type"] == "rate_limited" {
			sawRateLimited = true

			break
		}
	}

	assert.True(t, sawRateLimited, "error.type should be rate_limited when stop_reason mentions rate limit")
}

func TestOTelRecorder_ObserveResultDurationIncludesModelLabel(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	recorder := newOTelRecorder(metrics.Provider(), nil)
	require.NotNil(t, recorder)

	// Cache model via assistant message first.
	recorder.Observe(context.Background(), &message.AssistantMessage{Model: testModel})

	result := &message.ResultMessage{
		DurationMs: 2000,
		Usage:      &message.Usage{InputTokens: 100, OutputTokens: 50},
	}

	recorder.Observe(context.Background(), result)

	points, err := metrics.HistogramPoints(context.Background(), metricOperationDuration)
	require.NoError(t, err)
	require.NotEmpty(t, points)

	var sawModel bool

	for _, p := range points {
		if p.Attributes["gen_ai.request.model"] == testModel {
			sawModel = true

			break
		}
	}

	assert.True(t, sawModel, "duration metric should include gen_ai.request.model attribute")
}

func TestOTelRecorder_ToolCallCorrelation(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	recorder := newOTelRecorder(metrics.Provider(), traces.Provider())
	require.NotNil(t, recorder)

	ctx := context.Background()

	recorder.Observe(ctx, &message.AssistantMessage{
		Model: testModel,
		Content: []message.ContentBlock{
			&message.ToolUseBlock{
				Type:  message.BlockTypeToolUse,
				ID:    "call_1",
				Name:  "Bash",
				Input: map[string]any{"command": "ls"},
			},
		},
	})

	recorder.Observe(ctx, &message.UserMessage{
		Content: message.NewUserMessageContentBlocks([]message.ContentBlock{
			&message.ToolResultBlock{
				Type:      message.BlockTypeToolResult,
				ToolUseID: "call_1",
				IsError:   false,
			},
		}),
	})

	toolPoints, err := metrics.Int64Points(ctx, metricToolCallsTotal)
	require.NoError(t, err)
	require.NotEmpty(t, toolPoints, "tool_calls_total should be recorded")
	assert.Equal(t, "Bash", toolPoints[0].Attributes["gen_ai.tool.name"])
	assert.Equal(t, "ok", toolPoints[0].Attributes["outcome"])

	durationPoints, err := metrics.HistogramPoints(ctx, metricToolCallDuration)
	require.NoError(t, err)
	require.NotEmpty(t, durationPoints, "tool_call_duration should be recorded")

	summaries := traces.Summaries()
	require.NotEmpty(t, summaries, "tool span should be ended")

	var sawToolSpan bool

	for _, s := range summaries {
		if s.Name == "execute_tool Bash" {
			sawToolSpan = true

			break
		}
	}

	assert.True(t, sawToolSpan, "expected execute_tool span")
}

func TestOTelRecorder_ToolCallError(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	recorder := newOTelRecorder(metrics.Provider(), nil)
	require.NotNil(t, recorder)

	ctx := context.Background()

	recorder.Observe(ctx, &message.AssistantMessage{
		Model: testModel,
		Content: []message.ContentBlock{
			&message.ToolUseBlock{Type: message.BlockTypeToolUse, ID: "call_err", Name: "Write"},
		},
	})

	recorder.Observe(ctx, &message.UserMessage{
		Content: message.NewUserMessageContentBlocks([]message.ContentBlock{
			&message.ToolResultBlock{
				Type:      message.BlockTypeToolResult,
				ToolUseID: "call_err",
				IsError:   true,
			},
		}),
	})

	points, err := metrics.Int64Points(ctx, metricToolCallsTotal)
	require.NoError(t, err)
	require.NotEmpty(t, points)
	assert.Equal(t, "error", points[0].Attributes["outcome"])
}

// TestOTelRecorder_ToolCallColocated verifies that when ToolUseBlock and
// ToolResultBlock appear in the same AssistantMessage (Codex item.completed),
// the tool span is opened and immediately closed with correct metrics.
func TestOTelRecorder_ToolCallColocated(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	recorder := newOTelRecorder(metrics.Provider(), traces.Provider())
	require.NotNil(t, recorder)

	ctx := context.Background()

	// Single AssistantMessage with both ToolUseBlock and ToolResultBlock.
	recorder.Observe(ctx, &message.AssistantMessage{
		Model: testModel,
		Content: []message.ContentBlock{
			&message.ToolUseBlock{
				Type:  message.BlockTypeToolUse,
				ID:    "call_coloc",
				Name:  "Bash",
				Input: map[string]any{"command": "echo hello"},
			},
			&message.ToolResultBlock{
				Type:      message.BlockTypeToolResult,
				ToolUseID: "call_coloc",
				IsError:   false,
			},
		},
	})

	// Tool call counter should be recorded.
	toolPoints, err := metrics.Int64Points(ctx, metricToolCallsTotal)
	require.NoError(t, err)
	require.NotEmpty(t, toolPoints, "tool_calls_total should be recorded for co-located blocks")
	assert.Equal(t, "Bash", toolPoints[0].Attributes["gen_ai.tool.name"])
	assert.Equal(t, "ok", toolPoints[0].Attributes["outcome"])

	// Tool span should be ended.
	summaries := traces.Summaries()
	require.NotEmpty(t, summaries, "tool span should be ended")

	var sawToolSpan bool

	for _, s := range summaries {
		if s.Name == "execute_tool Bash" {
			sawToolSpan = true

			break
		}
	}

	assert.True(t, sawToolSpan, "expected execute_tool span for co-located blocks")

	// No leaked tool state.
	assert.Empty(t, recorder.tools, "tool state should be cleaned up")
}

func TestOTelRecorder_TTFTRecordedOnFirstAssistant(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	recorder := newOTelRecorder(metrics.Provider(), nil)
	require.NotNil(t, recorder)

	recorder.markQueryStart()

	ctx := context.Background()
	recorder.Observe(ctx, &message.AssistantMessage{Model: testModel})
	// Second assistant message should not record a second TTFT.
	recorder.Observe(ctx, &message.AssistantMessage{Model: testModel})

	points, err := metrics.HistogramPoints(ctx, metricTTFT)
	require.NoError(t, err)
	require.Len(t, points, 1, "TTFT should be recorded exactly once")
	assert.Equal(t, testModel, points[0].Attributes["gen_ai.request.model"])
}

func TestInitMetricsRecorder(t *testing.T) {
	t.Parallel()

	t.Run("nil options", func(t *testing.T) {
		t.Parallel()

		initMetricsRecorder(nil)
	})

	t.Run("no providers", func(t *testing.T) {
		t.Parallel()

		options := &config.Options{}
		initMetricsRecorder(options)
		assert.Nil(t, options.MetricsRecorder)
	})

	t.Run("with meter provider", func(t *testing.T) {
		t.Parallel()

		metrics := testkit.NewMetricsHarness()

		t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

		options := &config.Options{MeterProvider: metrics.Provider()}
		initMetricsRecorder(options)
		assert.NotNil(t, options.MetricsRecorder)
	})

	t.Run("already set", func(t *testing.T) {
		t.Parallel()

		metrics := testkit.NewMetricsHarness()

		t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

		existing := newOTelRecorder(metrics.Provider(), nil)
		options := &config.Options{
			MeterProvider:   metrics.Provider(),
			MetricsRecorder: existing,
		}
		initMetricsRecorder(options)
		assert.Equal(t, existing, options.MetricsRecorder)
	})
}

func TestStartQuerySpan_NoTracer(t *testing.T) {
	t.Parallel()

	options := &config.Options{}

	ctx := context.Background()
	newCtx, span := startQuerySpan(ctx, options, "query")

	assert.Equal(t, ctx, newCtx)
	assert.False(t, span.IsRecording())

	span.End()
}

func TestStartQuerySpan_WithTracer(t *testing.T) {
	t.Parallel()

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	options := &config.Options{
		TracerProvider: traces.Provider(),
		Model:          testModel,
	}

	initMetricsRecorder(options)

	ctx := context.Background()
	_, span := startQuerySpan(ctx, options, "query")
	span.End()

	summaries := traces.Summaries()
	require.Len(t, summaries, 1)
	assert.Equal(t, "chat codex-model", summaries[0].Name)
	assert.Equal(t, testModel, summaries[0].Attributes["gen_ai.request.model"])
	assert.Equal(t, "chat", summaries[0].Attributes["gen_ai.operation.name"])
	assert.Equal(t, "codex-cli", summaries[0].Attributes["gen_ai.provider.name"])
}

func TestWithMeterProviderOption(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	options := applyAgentOptions([]Option{WithMeterProvider(metrics.Provider())})
	assert.Equal(t, metrics.Provider(), options.MeterProvider)
}

func TestWithTracerProviderOption(t *testing.T) {
	t.Parallel()

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	options := applyAgentOptions([]Option{WithTracerProvider(traces.Provider())})
	assert.Equal(t, traces.Provider(), options.TracerProvider)
}

func TestSessionMetricsRecorderInterface(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	recorder := newOTelRecorder(metrics.Provider(), nil)

	var iface config.SessionMetricsRecorder = recorder
	assert.NotNil(t, iface)

	iface.Observe(context.Background(), &message.ResultMessage{
		Usage: &message.Usage{InputTokens: 10, OutputTokens: 5},
	})
}

// TestExemplarsAttachedToHistograms verifies that histogram points recorded
// inside a sampled trace context carry trace-based exemplars.
func TestExemplarsAttachedToHistograms(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithExemplarFilter(exemplar.TraceBasedFilter),
	)

	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))

	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	options := &config.Options{
		MeterProvider:  mp,
		TracerProvider: tp,
		Model:          testModel,
	}
	initMetricsRecorder(options)

	ctx, span := startQuerySpan(context.Background(), options, "query")
	wantTraceID := span.SpanContext().TraceID()
	wantSpanID := span.SpanContext().SpanID()

	options.MetricsRecorder.Observe(ctx, &message.ResultMessage{
		DurationMs: 420,
		Usage:      &message.Usage{InputTokens: 10, OutputTokens: 5},
	})

	span.End()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	var (
		sawHistogram bool
		sawExemplar  bool
		gotTraceID   []byte
		gotSpanID    []byte
	)

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != metricOperationDuration {
				continue
			}

			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}

			sawHistogram = true

			for _, dp := range hist.DataPoints {
				for _, ex := range dp.Exemplars {
					sawExemplar = true
					gotTraceID = ex.TraceID
					gotSpanID = ex.SpanID
				}
			}
		}
	}

	require.True(t, sawHistogram, "expected operation-duration histogram")
	require.True(t, sawExemplar, "expected at least one exemplar on the histogram")
	assert.Equal(t, wantTraceID[:], gotTraceID)
	assert.Equal(t, wantSpanID[:], gotSpanID)
}

func TestOTelRecorder_ObserveResultEnrichesSpan(t *testing.T) {
	t.Parallel()

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	options := &config.Options{
		TracerProvider: traces.Provider(),
		Model:          testModel,
	}
	initMetricsRecorder(options)

	// Cache model via assistant message first.
	ctx, span := startQuerySpan(context.Background(), options, "query")
	options.MetricsRecorder.Observe(ctx, &message.AssistantMessage{Model: testModel})

	stopReason := "end_turn"
	result := &message.ResultMessage{
		DurationMs: 1000,
		StopReason: &stopReason,
		Usage:      &message.Usage{InputTokens: 100, OutputTokens: 50},
	}

	options.MetricsRecorder.Observe(ctx, result)

	span.End()

	summaries := traces.Summaries()
	require.Len(t, summaries, 1)
	assert.Equal(t, testModel, summaries[0].Attributes["gen_ai.response.model"])
	assert.Equal(t, "[\"end_turn\"]", summaries[0].Attributes["gen_ai.response.finish_reasons"])
}

func TestOTelRecorder_ObserveResultSetsSpanErrorStatus(t *testing.T) {
	t.Parallel()

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	options := &config.Options{
		TracerProvider: traces.Provider(),
		Model:          testModel,
	}
	initMetricsRecorder(options)

	stopReason := "rate limit exceeded"

	result := &message.ResultMessage{
		DurationMs: 500,
		IsError:    true,
		StopReason: &stopReason,
		Usage:      &message.Usage{InputTokens: 50},
	}

	ctx, span := startQuerySpan(context.Background(), options, "query")
	options.MetricsRecorder.Observe(ctx, result)

	span.End()

	ended := traces.Ended()
	require.Len(t, ended, 1)

	assert.Equal(t, codes.Error, ended[0].Status().Code)
	assert.Equal(t, "rate_limited", ended[0].Status().Description)

	// Verify error.type attribute is set on the span.
	var sawErrorType bool

	for _, attr := range ended[0].Attributes() {
		if string(attr.Key) == "error.type" && attr.Value.AsString() == "rate_limited" {
			sawErrorType = true

			break
		}
	}

	assert.True(t, sawErrorType, "span should have error.type=rate_limited attribute")
}

func TestOTelRecorder_ObserveResultNoErrorStatusOnSuccess(t *testing.T) {
	t.Parallel()

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	options := &config.Options{
		TracerProvider: traces.Provider(),
		Model:          testModel,
	}
	initMetricsRecorder(options)

	result := &message.ResultMessage{
		DurationMs: 1000,
		Usage:      &message.Usage{InputTokens: 100, OutputTokens: 50},
	}

	ctx, span := startQuerySpan(context.Background(), options, "query")
	options.MetricsRecorder.Observe(ctx, result)

	span.End()

	ended := traces.Ended()
	require.Len(t, ended, 1)

	// Span status should be unset (not Error, not Ok) per GenAI spec.
	assert.Equal(t, codes.Unset, ended[0].Status().Code,
		"successful result should not set span status")
}

func TestApplyAgentOptionsToConfig_InitializesRecorder(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	options := applyAgentOptionsToConfig([]Option{
		WithMeterProvider(metrics.Provider()),
	})

	require.NotNil(t, options)
	assert.NotNil(t, options.MetricsRecorder)
	assert.NotNil(t, options.Observer)
}

// metricNames returns the set of metric names observed by the harness.
func metricNames(t *testing.T, metrics *testkit.MetricsHarness) map[string]bool {
	t.Helper()

	names, err := metrics.MetricNames(context.Background())
	require.NoError(t, err)

	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}

	return set
}
