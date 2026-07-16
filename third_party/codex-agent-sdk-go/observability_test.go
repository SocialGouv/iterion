package codexsdk

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
)

// TestWithMeterProvider verifies the option sets the field.
func TestWithMeterProvider(t *testing.T) {
	t.Parallel()

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	defer func() { _ = mp.Shutdown(context.Background()) }()

	opts := applyAgentOptions([]Option{WithMeterProvider(mp)})
	require.Equal(t, mp, opts.MeterProvider)
}

// TestWithTracerProvider verifies the option sets the field.
func TestWithTracerProvider(t *testing.T) {
	t.Parallel()

	tp := sdktrace.NewTracerProvider()

	defer func() { _ = tp.Shutdown(context.Background()) }()

	opts := applyAgentOptions([]Option{WithTracerProvider(tp)})
	require.Equal(t, tp, opts.TracerProvider)
}

// TestWithPrometheusRegisterer verifies the option sets the field.
func TestWithPrometheusRegisterer(t *testing.T) {
	t.Parallel()

	// Use nil registerer just to test the option mechanism.
	opts := applyAgentOptions([]Option{WithPrometheusRegisterer(nil)})
	require.Nil(t, opts.PrometheusRegisterer)
}

// TestObservabilityOptions_DefaultNil verifies options default to nil.
func TestObservabilityOptions_DefaultNil(t *testing.T) {
	t.Parallel()

	opts := &CodexAgentOptions{}
	require.Nil(t, opts.MeterProvider)
	require.Nil(t, opts.TracerProvider)
	require.Nil(t, opts.PrometheusRegisterer)
}

// TestObservabilityOptions_BackendSupport verifies observability options
// are supported on both backends.
func TestObservabilityOptions_BackendSupport(t *testing.T) {
	t.Parallel()

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	defer func() { _ = mp.Shutdown(context.Background()) }()

	tp := sdktrace.NewTracerProvider()

	defer func() { _ = tp.Shutdown(context.Background()) }()

	opts := &CodexAgentOptions{
		MeterProvider:  mp,
		TracerProvider: tp,
	}

	// Both backends should accept observability options.
	require.NoError(t, config.ValidateOptionsForBackend(opts, config.QueryBackendExec))
	require.NoError(t, config.ValidateOptionsForBackend(opts, config.QueryBackendAppServer))
}

// TestObservabilityOptions_DoNotForceAppServer verifies that observability
// options alone do not force the app-server backend.
func TestObservabilityOptions_DoNotForceAppServer(t *testing.T) {
	t.Parallel()

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	defer func() { _ = mp.Shutdown(context.Background()) }()

	opts := &CodexAgentOptions{
		MeterProvider: mp,
	}

	backend := config.SelectQueryBackend(opts)
	require.Equal(t, config.QueryBackendExec, backend)
}

// otelResultTransport is a mock transport that emits a ResultMessage with
// token usage for testing observability recording.
type otelResultTransport struct {
	mu         sync.Mutex
	msgChan    chan map[string]any
	errChan    chan error
	sentResult atomic.Bool
	closed     bool
}

func newOtelResultTransport() *otelResultTransport {
	return &otelResultTransport{
		msgChan: make(chan map[string]any, 16),
		errChan: make(chan error, 1),
	}
}

func (t *otelResultTransport) Start(_ context.Context) error {
	return nil
}

func (t *otelResultTransport) ReadMessages(
	_ context.Context,
) (<-chan map[string]any, <-chan error) {
	return t.msgChan, t.errChan
}

func (t *otelResultTransport) SendMessage(
	_ context.Context,
	data []byte,
) error {
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}

	if msgType, ok := msg["type"].(string); ok && msgType == msgTypeControlRequest {
		requestID, _ := msg["request_id"].(string)
		if requestID == "" {
			return nil
		}

		go func() {
			t.msgChan <- map[string]any{
				"type": "control_response",
				"response": map[string]any{
					"subtype":    "success",
					"request_id": requestID,
					"response":   map[string]any{},
				},
			}
		}()

		return nil
	}

	if t.sentResult.CompareAndSwap(false, true) {
		go func() {
			t.msgChan <- map[string]any{
				"type":        "result",
				"subtype":     "success",
				"is_error":    false,
				"session_id":  "session-otel-test",
				"duration_ms": float64(1500),
				"usage": map[string]any{
					"input_tokens":  float64(100),
					"output_tokens": float64(50),
				},
			}
		}()
	}

	return nil
}

func (t *otelResultTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.closed {
		t.closed = true
		close(t.msgChan)
		close(t.errChan)
	}

	return nil
}

func (t *otelResultTransport) IsReady() bool {
	return true
}

func (t *otelResultTransport) EndInput() error {
	if t.sentResult.CompareAndSwap(false, true) {
		go func() {
			t.msgChan <- map[string]any{
				"type":        "result",
				"subtype":     "success",
				"is_error":    false,
				"session_id":  "session-otel-test",
				"duration_ms": float64(1500),
				"usage": map[string]any{
					"input_tokens":  float64(100),
					"output_tokens": float64(50),
				},
			}
		}()
	}

	return nil
}

var _ config.Transport = (*otelResultTransport)(nil)

// TestQuery_RecordsOperationDuration verifies that Query records the
// gen_ai.client.operation.duration histogram when a MeterProvider is set.
func TestQuery_RecordsOperationDuration(t *testing.T) {
	t.Parallel()

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	defer func() { _ = mp.Shutdown(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	transport := newOtelResultTransport()

	for msg, err := range Query(ctx, Text("test"),
		WithMeterProvider(mp),
		WithTransport(transport),
	) {
		require.NoError(t, err)

		if _, ok := msg.(*ResultMessage); ok {
			break
		}
	}

	var rm metricdata.ResourceMetrics

	require.NoError(t, reader.Collect(ctx, &rm))
	require.NotEmpty(t, rm.ScopeMetrics)

	found := false

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "gen_ai.client.operation.duration" {
				found = true
			}
		}
	}

	require.True(t, found, "expected gen_ai.client.operation.duration metric")
}

// TestQuery_RecordsTokenUsage verifies that Query records the
// gen_ai.client.token.usage counter when a MeterProvider is set.
func TestQuery_RecordsTokenUsage(t *testing.T) {
	t.Parallel()

	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	defer func() { _ = mp.Shutdown(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	transport := newOtelResultTransport()

	for msg, err := range Query(ctx, Text("test"),
		WithMeterProvider(mp),
		WithTransport(transport),
	) {
		require.NoError(t, err)

		if _, ok := msg.(*ResultMessage); ok {
			break
		}
	}

	var rm metricdata.ResourceMetrics

	require.NoError(t, reader.Collect(ctx, &rm))
	require.NotEmpty(t, rm.ScopeMetrics)

	found := false

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "gen_ai.client.token.usage" {
				found = true
			}
		}
	}

	require.True(t, found, "expected gen_ai.client.token.usage metric")
}

// TestQuery_CreatesSpan verifies that Query creates a span when a
// TracerProvider is set.
func TestQuery_CreatesSpan(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	transport := newOtelResultTransport()

	for msg, err := range Query(ctx, Text("test"),
		WithTracerProvider(tp),
		WithTransport(transport),
	) {
		require.NoError(t, err)

		if _, ok := msg.(*ResultMessage); ok {
			break
		}
	}

	spans := exporter.GetSpans()
	require.NotEmpty(t, spans)

	found := false

	for _, span := range spans {
		if span.Name == "chat" {
			found = true

			break
		}
	}

	require.True(t, found, "expected chat span")
}

// TestQuery_NoProviders_NoPanic verifies that Query works correctly
// when no observability providers are configured.
func TestQuery_NoProviders_NoPanic(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	transport := newOtelResultTransport()

	for msg, err := range Query(ctx, Text("test"),
		WithTransport(transport),
	) {
		require.NoError(t, err)

		if _, ok := msg.(*ResultMessage); ok {
			break
		}
	}
}
