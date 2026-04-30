// Package otlpgrpc implements an OTLP/gRPC exporter for apikit.TelemetryEvent
// using the official OpenTelemetry SDK (sdk/log + otlploggrpc). Events are
// translated to OTLP LogRecords and pushed through a BatchProcessor that
// dispatches to the otlploggrpc Exporter — batching, retry on transient
// gRPC errors, and shutdown drain are inherited from the SDK.
//
// API parity with the sibling otlp HTTP/JSON exporter is intentional: both
// types satisfy apikit.TelemetrySink so callers can swap transports without
// touching the recording call sites.
package otlpgrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/SocialGouv/claw-code-go/internal/apikit"
)

// EnvEndpoint is the environment variable consulted by FromEnv to discover
// the collector endpoint. Standard OpenTelemetry tooling honors
// OTEL_EXPORTER_OTLP_ENDPOINT — we keep the project-prefixed name as the
// primary so operators can set it independently of any other OTel SDK
// elsewhere in the process.
const EnvEndpoint = "CLAWD_OTLP_GRPC_ENDPOINT"

// Defaults for batching. Smaller than the HTTP defaults because gRPC keeps
// a long-lived connection and protobuf framing is cheap; flushing more
// often keeps latency low without paying per-request HTTP overhead.
const (
	DefaultBatchSize     = 512
	DefaultFlushInterval = 5 * time.Second
	DefaultExportTimeout = 30 * time.Second
)

// Severity values mirror the HTTP exporter. We only emit two — INFO for
// normal traffic, ERROR for failed HTTP calls — to stay aligned with the
// OTLP logs data model and the HTTP exporter wire format.
const (
	severityInfo  = otellog.SeverityInfo
	severityError = otellog.SeverityError
)

// Config configures the gRPC exporter. Endpoint is the only required
// field. Defaults are chosen to be safe (insecure off in production
// requires an explicit Insecure flag), and tests can override every knob.
type Config struct {
	// Endpoint is the OTLP/gRPC collector address ("host:port"), or a
	// URL ("https://host:4317"). Empty endpoint is rejected.
	Endpoint string

	// Headers are attached to every gRPC request — e.g. auth tokens.
	Headers map[string]string

	// ServiceName populates the resource service.name attribute.
	// Defaults to "claw-code-go".
	ServiceName string

	// ServiceVersion populates the resource service.version attribute
	// when set.
	ServiceVersion string

	// Insecure disables TLS. Required when targeting a plaintext local
	// collector (the default OTel exporter uses TLS otherwise).
	Insecure bool

	// BatchSize triggers a flush once the in-memory queue reaches this
	// many records. Defaults to DefaultBatchSize.
	BatchSize int

	// FlushInterval triggers a flush at most this often regardless of
	// queue depth. Defaults to DefaultFlushInterval.
	FlushInterval time.Duration

	// ExportTimeout caps a single export RPC. Defaults to
	// DefaultExportTimeout.
	ExportTimeout time.Duration

	// ExtraExporterOptions are appended verbatim to the otlploggrpc
	// New() option slice. Tests inject WithGRPCConn(bufconn) here.
	ExtraExporterOptions []otlploggrpc.Option

	// ErrorHandler observes export failures so operators can wire
	// alerts. If nil, errors propagate via Start / Stop returns where
	// possible but in-flight async batch failures are dropped (matching
	// the HTTP sibling).
	ErrorHandler func(error)
}

// Exporter satisfies apikit.TelemetrySink and ships events to an
// OTLP/gRPC collector. Stop must be called for a clean shutdown — it
// drains the batch processor and closes the underlying gRPC client.
type Exporter struct {
	cfg            Config
	provider       *sdklog.LoggerProvider
	logger         otellog.Logger
	batchProcessor *sdklog.BatchProcessor
	otlpExporter   *otlploggrpc.Exporter

	mu      sync.RWMutex
	stopped bool

	stopOnce sync.Once
}

// New constructs a gRPC exporter and the underlying OTLP client. It does
// not start any background work — Start is a no-op kept for API parity
// with the HTTP sibling.
func New(cfg Config) (*Exporter, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("otlpgrpc: Endpoint is required")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultFlushInterval
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = DefaultExportTimeout
	}
	if strings.TrimSpace(cfg.ServiceName) == "" {
		cfg.ServiceName = "claw-code-go"
	}

	opts := []otlploggrpc.Option{
		otlploggrpc.WithEndpoint(cfg.Endpoint),
		otlploggrpc.WithTimeout(cfg.ExportTimeout),
	}
	if cfg.Insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlploggrpc.WithHeaders(cfg.Headers))
	}
	opts = append(opts, cfg.ExtraExporterOptions...)

	otlpExp, err := otlploggrpc.New(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("otlpgrpc: build exporter: %w", err)
	}

	bp := sdklog.NewBatchProcessor(otlpExp,
		sdklog.WithExportMaxBatchSize(cfg.BatchSize),
		sdklog.WithExportInterval(cfg.FlushInterval),
		sdklog.WithExportTimeout(cfg.ExportTimeout),
	)

	resAttrs := []attribute.KeyValue{
		attribute.String("service.name", cfg.ServiceName),
	}
	if cfg.ServiceVersion != "" {
		resAttrs = append(resAttrs, attribute.String("service.version", cfg.ServiceVersion))
	}
	res := resource.NewSchemaless(resAttrs...)

	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(bp),
	)
	logger := provider.Logger("claw-code-go")

	return &Exporter{
		cfg:            cfg,
		provider:       provider,
		logger:         logger,
		batchProcessor: bp,
		otlpExporter:   otlpExp,
	}, nil
}

// Start is a no-op kept for symmetry with the HTTP exporter. Background
// flushing is owned by the SDK BatchProcessor created in New.
func (e *Exporter) Start(ctx context.Context) error { return nil }

// Record translates a TelemetryEvent into an OTLP LogRecord and emits
// it via the SDK Logger. Safe for concurrent use; never blocks longer
// than the BatchProcessor enqueue.
func (e *Exporter) Record(event apikit.TelemetryEvent) {
	e.mu.RLock()
	stopped := e.stopped
	e.mu.RUnlock()
	if stopped {
		return
	}

	rec := eventToRecord(event)
	e.logger.Emit(context.Background(), rec)
}

// Stop drains the batch processor, shuts down the gRPC exporter, and is
// idempotent. Concurrent calls all wait for the same shutdown — only the
// first does work; subsequent calls return the same drain status.
func (e *Exporter) Stop(ctx context.Context) error {
	var err error
	e.stopOnce.Do(func() {
		e.mu.Lock()
		e.stopped = true
		e.mu.Unlock()

		if shutErr := e.provider.Shutdown(ctx); shutErr != nil {
			err = shutErr
			if e.cfg.ErrorHandler != nil {
				e.cfg.ErrorHandler(shutErr)
			}
		}
	})
	return err
}

// ForceFlush exports pending records synchronously. Useful at well-known
// drain points (test cleanup, post-action commits) without taking down
// the exporter.
func (e *Exporter) ForceFlush(ctx context.Context) error {
	return e.provider.ForceFlush(ctx)
}

func eventToRecord(ev apikit.TelemetryEvent) otellog.Record {
	var rec otellog.Record
	now := time.Now()
	rec.SetTimestamp(now)
	rec.SetObservedTimestamp(now)
	rec.SetSeverity(severityForEvent(ev))
	rec.SetSeverityText(severityTextForEvent(ev))

	body, err := json.Marshal(ev)
	if err != nil {
		body = []byte(fmt.Sprintf(`{"type":%q,"marshal_error":%q}`, ev.Type, err.Error()))
	}
	rec.SetBody(otellog.StringValue(string(body)))

	rec.AddAttributes(otellog.String("event.type", string(ev.Type)))
	if ev.SessionID != "" {
		rec.AddAttributes(otellog.String("session.id", ev.SessionID))
	}
	if ev.Method != "" {
		rec.AddAttributes(otellog.String("http.method", ev.Method))
	}
	if ev.Path != "" {
		rec.AddAttributes(otellog.String("http.path", ev.Path))
	}
	if ev.Status != 0 {
		rec.AddAttributes(otellog.Int("http.status_code", int(ev.Status)))
	}
	if ev.RequestID != "" {
		rec.AddAttributes(otellog.String("http.request_id", ev.RequestID))
	}
	if ev.Error != "" {
		rec.AddAttributes(otellog.String("error.message", ev.Error))
		rec.AddAttributes(otellog.Bool("error.retryable", ev.Retryable))
	}
	return rec
}

func severityForEvent(ev apikit.TelemetryEvent) otellog.Severity {
	if ev.Type == apikit.EventTypeHTTPRequestFailed {
		return severityError
	}
	return severityInfo
}

func severityTextForEvent(ev apikit.TelemetryEvent) string {
	if ev.Type == apikit.EventTypeHTTPRequestFailed {
		return "ERROR"
	}
	return "INFO"
}
