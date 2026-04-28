// Package tracer wraps an OpenTelemetry Tracer with a noop default and an
// errclass Registry so a single RecordError call can classify, record, and
// set span status. The package deliberately stays thin — SDKs add their own
// higher-level helpers (events, attributes) on top of this primitive.
package tracer

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"

	"github.com/ethpandaops/agent-sdk-observability/errclass"
)

// Recorder wraps a trace.Tracer and an errclass.Registry.
type Recorder struct {
	tracer  trace.Tracer
	classes *errclass.Registry
}

// New returns a Recorder. A nil provider falls back to the noop TracerProvider,
// a nil classes registry causes all recorded errors to be labeled Unknown.
func New(provider trace.TracerProvider, scopeName, scopeVersion string, classes *errclass.Registry) *Recorder {
	if provider == nil {
		provider = nooptrace.NewTracerProvider()
	}

	return &Recorder{
		tracer:  provider.Tracer(scopeName, trace.WithInstrumentationVersion(scopeVersion)),
		classes: classes,
	}
}

// Tracer returns the underlying OTel Tracer for code that needs direct access.
func (r *Recorder) Tracer() trace.Tracer {
	if r == nil {
		return nooptrace.NewTracerProvider().Tracer("")
	}

	return r.tracer
}

// Start opens a span. A nil receiver is safe and returns a noop span.
func (r *Recorder) Start(
	ctx context.Context,
	name string,
	kind trace.SpanKind,
	attrs ...attribute.KeyValue,
) (context.Context, *Span) {
	if r == nil {
		r = New(nil, "", "", nil)
	}

	ctx, span := r.tracer.Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(attrs...))

	return ctx, &Span{span: span, classes: r.classes}
}

// Span wraps trace.Span with errclass integration. All methods are safe on nil.
type Span struct {
	span    trace.Span
	classes *errclass.Registry
}

// End finalizes the span.
func (s *Span) End() {
	if s == nil || s.span == nil {
		return
	}

	s.span.End()
}

// SetAttributes adds attributes to the span.
func (s *Span) SetAttributes(attrs ...attribute.KeyValue) {
	if s == nil || s.span == nil {
		return
	}

	s.span.SetAttributes(attrs...)
}

// AddEvent adds a named event with optional attributes.
func (s *Span) AddEvent(name string, attrs ...attribute.KeyValue) {
	if s == nil || s.span == nil {
		return
	}

	s.span.AddEvent(name, trace.WithAttributes(attrs...))
}

// SpanContext returns the underlying span context (useful for exemplar linking).
func (s *Span) SpanContext() trace.SpanContext {
	if s == nil || s.span == nil {
		return trace.SpanContext{}
	}

	return s.span.SpanContext()
}

// Raw returns the underlying OTel Span. Prefer the wrapper methods where possible.
func (s *Span) Raw() trace.Span {
	if s == nil {
		return nil
	}

	return s.span
}

// RecordError classifies err via the registry, records it on the span, sets
// the error.type attribute, and marks the span status as error. Returns the
// classified Class so callers can reuse it for metric labels without double
// classification.
func (s *Span) RecordError(err error) errclass.Class {
	if s == nil || s.span == nil || err == nil {
		return ""
	}

	class := errclass.Unknown
	if s.classes != nil {
		class = s.classes.Classify(err)
	}

	attr := class.Attr()
	s.span.RecordError(err, trace.WithAttributes(attr))
	s.span.SetAttributes(attr)
	s.span.SetStatus(codes.Error, string(class))

	return class
}

// MarkError flags the span as failed with a pre-classified Class but without a
// backing error value (e.g., a hook gated the call, no exception to capture).
func (s *Span) MarkError(class errclass.Class) {
	if s == nil || s.span == nil || class == "" {
		return
	}

	attr := class.Attr()
	s.span.SetAttributes(attr)
	s.span.SetStatus(codes.Error, string(class))
}
