package nats

import (
	"context"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/SocialGouv/iterion/pkg/queue"
)

// natsHeaderCarrier adapts nats.Header to propagation.TextMapCarrier
// so the W3C TraceContext propagator can set / get the standard
// `traceparent` + `tracestate` headers without additional plumbing.
//
// Plan §F (T-41).
type natsHeaderCarrier nats.Header

func (c natsHeaderCarrier) Get(key string) string    { return nats.Header(c).Get(key) }
func (c natsHeaderCarrier) Set(key, value string)    { nats.Header(c).Set(key, value) }
func (c natsHeaderCarrier) Keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}

// injectTrace stamps the W3C traceparent + tracestate headers from
// the supplied ctx into the message header. It also fills the
// queue.TraceContext mirror so consumers that decode the body can
// recover the trace without parsing headers themselves (defence in
// depth — the plan §C.2 lists both).
func injectTrace(ctx context.Context, msg *queue.RunMessage, headers nats.Header) {
	otel.GetTextMapPropagator().Inject(ctx, natsHeaderCarrier(headers))
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		sc := span.SpanContext()
		msg.Trace.TraceID = sc.TraceID().String()
		msg.Trace.SpanID = sc.SpanID().String()
	}
}

// extractTrace recovers a SpanContext from the NATS headers and
// returns a child context the runner can pass to engine.Run. When
// no headers are present (legacy publishers, local-mode tests) the
// original ctx is returned unchanged so callers can use the result
// unconditionally.
func extractTrace(ctx context.Context, headers nats.Header) context.Context {
	if headers == nil {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, natsHeaderCarrier(headers))
}

// EnsureDefaultPropagator installs propagation.TraceContext as the
// global propagator if no propagator was set yet. Safe to call from
// multiple init paths — the first call wins; subsequent calls are
// no-ops because we read the global before assigning.
func EnsureDefaultPropagator() {
	if _, ok := otel.GetTextMapPropagator().(propagation.TraceContext); ok {
		return
	}
	otel.SetTextMapPropagator(propagation.TraceContext{})
}
