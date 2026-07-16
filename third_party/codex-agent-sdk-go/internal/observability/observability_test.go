package observability

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/ethpandaops/agent-sdk-observability/testkit"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sdkerrors "github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
)

func TestNew_NilProviders(t *testing.T) {
	t.Parallel()

	obs, err := New(Config{})
	require.NoError(t, err)
	require.NotNil(t, obs)
}

func TestNoop(t *testing.T) {
	t.Parallel()

	obs := Noop()
	require.NotNil(t, obs)

	// Noop observer should not panic on any recording call.
	ctx := context.Background()
	obs.RecordOperationDuration(ctx, 1.0, "chat", "test-model", "")
	obs.RecordTokenUsage(ctx, 100, "input", "chat", "test-model")
	obs.RecordTTFT(ctx, 0.5, "test-model")
	obs.RecordCost(ctx, 0.001, "test-model")
	obs.RecordToolCall(ctx, "bash", "ok")
	obs.RecordToolCallDuration(ctx, 0.2, "bash")
	obs.RecordHookDuration(ctx, 0.1, "pre_tool_use", "ok")
}

func TestNew_WithMeterProvider(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	obs, err := New(Config{MeterProvider: metrics.Provider()})
	require.NoError(t, err)
	require.NotNil(t, obs)
}

func TestNew_WithTracerProvider(t *testing.T) {
	t.Parallel()

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	obs, err := New(Config{TracerProvider: traces.Provider()})
	require.NoError(t, err)
	require.NotNil(t, obs)
}

func TestObserver_RecordOperationDuration(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	obs, err := New(Config{MeterProvider: metrics.Provider()})
	require.NoError(t, err)

	obs.RecordOperationDuration(context.Background(), 1.5, "chat", "test-model", "")

	points, err := metrics.HistogramPoints(context.Background(), "gen_ai.client.operation.duration")
	require.NoError(t, err)
	require.NotEmpty(t, points, "operation duration should be recorded")
}

func TestObserver_RecordTokenUsage(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	obs, err := New(Config{MeterProvider: metrics.Provider()})
	require.NoError(t, err)

	obs.RecordTokenUsage(context.Background(), 100, "input", "chat", "test-model")

	names, err := metrics.MetricNames(context.Background())
	require.NoError(t, err)
	require.True(t, slices.Contains(names, "gen_ai.client.token.usage"), "token usage should be recorded")
}

func TestObserver_RecordTTFT(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	obs, err := New(Config{MeterProvider: metrics.Provider()})
	require.NoError(t, err)

	obs.RecordTTFT(context.Background(), 0.42, "test-model")

	points, err := metrics.HistogramPoints(context.Background(), "gen_ai.client.operation.time_to_first_chunk")
	require.NoError(t, err)
	require.NotEmpty(t, points, "TTFT should be recorded")
	assert.Equal(t, "test-model", points[0].Attributes["gen_ai.request.model"])
}

func TestObserver_RecordCost(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	obs, err := New(Config{MeterProvider: metrics.Provider()})
	require.NoError(t, err)

	obs.RecordCost(context.Background(), 0.0025, "test-model")

	// Zero cost should not record.
	obs.RecordCost(context.Background(), 0, "test-model")

	names, err := metrics.MetricNames(context.Background())
	require.NoError(t, err)
	require.True(t, slices.Contains(names, "gen_ai.client.cost_usd_total"), "cost metric should be recorded")
}

func TestObserver_RecordToolCall(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	obs, err := New(Config{MeterProvider: metrics.Provider()})
	require.NoError(t, err)

	obs.RecordToolCall(context.Background(), "bash", "ok")

	points, err := metrics.Int64Points(context.Background(), "codex.tool_calls_total")
	require.NoError(t, err)
	require.NotEmpty(t, points)
	assert.Equal(t, "bash", points[0].Attributes["gen_ai.tool.name"])
	assert.Equal(t, "ok", points[0].Attributes["outcome"])
}

func TestObserver_RecordToolCallDuration(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	obs, err := New(Config{MeterProvider: metrics.Provider()})
	require.NoError(t, err)

	obs.RecordToolCallDuration(context.Background(), 0.2, "bash")

	points, err := metrics.HistogramPoints(context.Background(), "codex.tool_call_duration")
	require.NoError(t, err)
	require.NotEmpty(t, points, "tool call duration should be recorded")
}

func TestObserver_RecordHookDuration(t *testing.T) {
	t.Parallel()

	metrics := testkit.NewMetricsHarness()

	t.Cleanup(func() { _ = metrics.Shutdown(context.Background()) })

	obs, err := New(Config{MeterProvider: metrics.Provider()})
	require.NoError(t, err)

	obs.RecordHookDuration(context.Background(), 0.1, "pre_tool_use", "ok")

	points, err := metrics.HistogramPoints(context.Background(), "codex.hook_dispatch_duration")
	require.NoError(t, err)
	require.NotEmpty(t, points, "hook dispatch duration should be recorded")
}

func TestObserver_StartQuerySpan(t *testing.T) {
	t.Parallel()

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	obs, err := New(Config{TracerProvider: traces.Provider()})
	require.NoError(t, err)

	_, span := obs.StartQuerySpan(context.Background(), "chat", "test-model", "session-1")
	span.End()

	summaries := traces.Summaries()
	require.Len(t, summaries, 1)
	assert.Equal(t, "chat test-model", summaries[0].Name)
	assert.Equal(t, "test-model", summaries[0].Attributes["gen_ai.request.model"])
	assert.Equal(t, "chat", summaries[0].Attributes["gen_ai.operation.name"])
	assert.Equal(t, "codex-cli", summaries[0].Attributes["gen_ai.provider.name"])
	assert.Equal(t, "session-1", summaries[0].Attributes["gen_ai.conversation.id"])
}

func TestObserver_StartToolSpan(t *testing.T) {
	t.Parallel()

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	obs, err := New(Config{TracerProvider: traces.Provider()})
	require.NoError(t, err)

	_, span := obs.StartToolSpan(context.Background(), "bash", "call_1")
	span.End()

	summaries := traces.Summaries()
	require.Len(t, summaries, 1)
	assert.Equal(t, "execute_tool bash", summaries[0].Name)
	assert.Equal(t, "bash", summaries[0].Attributes["gen_ai.tool.name"])
	assert.Equal(t, "call_1", summaries[0].Attributes["gen_ai.tool.call.id"])
}

func TestObserver_StartHookSpan(t *testing.T) {
	t.Parallel()

	traces := testkit.NewTracesHarness()

	t.Cleanup(func() { _ = traces.Shutdown(context.Background()) })

	obs, err := New(Config{TracerProvider: traces.Provider()})
	require.NoError(t, err)

	_, span := obs.StartHookSpan(context.Background(), "pre_tool_use")
	span.End()

	summaries := traces.Summaries()
	require.Len(t, summaries, 1)
	assert.Equal(t, "codex.hook.dispatch", summaries[0].Name)
	assert.Equal(t, "pre_tool_use", summaries[0].Attributes["hook.event"])
}

func TestClassify(t *testing.T) {
	t.Parallel()

	obs := Noop()

	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{name: "nil", err: nil, expected: ""},
		{name: "context_cancelled", err: context.Canceled, expected: "canceled"},
		{name: "context_deadline", err: context.DeadlineExceeded, expected: "timeout"},
		{name: "request_timeout", err: sdkerrors.ErrRequestTimeout, expected: "timeout"},
		{name: "operation_cancelled", err: sdkerrors.ErrOperationCancelled, expected: "canceled"},
		{name: "client_not_connected", err: sdkerrors.ErrClientNotConnected, expected: "network"},
		{name: "client_closed", err: sdkerrors.ErrClientClosed, expected: "network"},
		{name: "transport_not_connected", err: sdkerrors.ErrTransportNotConnected, expected: "network"},
		{name: "stdin_closed", err: sdkerrors.ErrStdinClosed, expected: "network"},
		{name: "controller_stopped", err: sdkerrors.ErrControllerStopped, expected: "network"},
		{
			name:     "cli_not_found",
			err:      &sdkerrors.CLINotFoundError{SearchedPaths: []string{"/usr/bin"}},
			expected: "cli_not_found",
		},
		{
			name:     "connection_error",
			err:      &sdkerrors.CLIConnectionError{Err: errors.New("conn failed")},
			expected: "network",
		},
		{
			name:     "process_error",
			err:      &sdkerrors.ProcessError{ExitCode: 1, Err: errors.New("exit 1")},
			expected: "process_error",
		},
		{
			name:     "message_parse_error",
			err:      &sdkerrors.MessageParseError{Err: errors.New("bad json")},
			expected: "parse_error",
		},
		{
			name:     "json_decode_error",
			err:      &sdkerrors.CLIJSONDecodeError{Err: errors.New("invalid json")},
			expected: "parse_error",
		},
		{
			name:     "rate_limit_string",
			err:      errors.New("rate limit exceeded"),
			expected: "rate_limited",
		},
		{
			name:     "overload_string",
			err:      errors.New("server overload"),
			expected: "overload",
		},
		{
			name:     "billing_string",
			err:      errors.New("billing error: insufficient credits"),
			expected: "billing",
		},
		{
			name:     "wrapped_timeout",
			err:      fmt.Errorf("wrapped: %w", sdkerrors.ErrRequestTimeout),
			expected: "timeout",
		},
		{name: "generic_error", err: errors.New("something unknown"), expected: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, string(obs.Classify(tt.err)))
		})
	}
}

func TestClassifyHTTPStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status   int
		expected string
	}{
		{401, "auth"},
		{403, "auth"},
		{402, "billing"},
		{429, "rate_limited"},
		{408, "timeout"},
		{503, "overload"},
		{529, "overload"},
		{500, "upstream_5xx"},
		{502, "upstream_5xx"},
		{400, "invalid_request"},
		{404, "invalid_request"},
		{200, ""},
		{301, ""},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("status_%d", tt.status), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, string(ClassifyHTTPStatus(tt.status)))
		})
	}
}

func TestNewPrometheusMeterProvider(t *testing.T) {
	t.Parallel()

	// Verifies the helper compiles and returns a valid provider from a real registry.
	reg := prometheus.NewRegistry()
	mp, err := NewPrometheusMeterProvider(reg)
	require.NoError(t, err)
	require.NotNil(t, mp, "should create MeterProvider from prometheus registerer")
}
