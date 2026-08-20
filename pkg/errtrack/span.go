package errtrack

import (
	"context"

	sentry "github.com/getsentry/sentry-go"
)

// Span is the handle StartSpan hands back. A nil *Span is the disabled
// case and every method tolerates it, so a call site is three lines
// with no `if` around them.
type Span struct{ span *sentry.Span }

// StartSpan times one unit of work and returns the context its children
// should use.
//
// **A nil Span and the caller's own ctx when tracing is off** — nothing
// is allocated, so a hot seam pays a single atomic load for carrying
// the instrumentation.
//
// op is the machine-facing operation ("llm.generate"); name is the
// low-cardinality label ("anthropic/claude-opus-5"). When ctx carries a
// parent span the result is a child span; otherwise it is a standalone
// transaction. ONLY use it for work that lives and dies INSIDE the
// parent's lifetime (a step of the HTTP request being traced): a child
// finished after its transaction was sent is silently dropped by the
// SDK, and a run launched from an API request keeps that request's
// (long-finished) transaction in its context — use StartIndependent for
// any background lifetime.
func StartSpan(ctx context.Context, op, name string) (context.Context, *Span) {
	if !tracing.Load() {
		return ctx, nil
	}
	s := sentry.StartSpan(ctx, op, sentry.WithTransactionName(op+" "+name))
	s.Description = name
	return s.Context(), &Span{span: s}
}

// StartIndependent times one unit of work as its OWN transaction,
// deliberately ignoring any span the caller's context may carry. This
// is the right shape for background lifetimes — a run's LLM call, a
// worker step — whose context is derived from a launch request: the
// request's transaction finished long ago, so parenting under it would
// orphan the span (never exported), and inspecting the finished span
// cross-goroutine would race. name must stay a bounded set (a
// provider/model), never a run id.
func StartIndependent(op, name string) *Span {
	if !tracing.Load() {
		return nil
	}
	// An isolated hub: sentry.StartSpan installs the new span on the
	// hub's scope and, for a transaction, never restores what was there
	// — so the process-global hub would leave every later error event
	// linked to a stale (or, under parallel generations, arbitrary)
	// llm.generate trace. The clone shares the client, so the
	// transaction still ships.
	ctx := sentry.SetHubOnContext(context.Background(), sentry.CurrentHub().Clone())
	s := sentry.StartSpan(ctx, op, sentry.WithTransactionName(op+" "+name))
	s.Description = name
	return &Span{span: s}
}

// SetData attaches a measurement to the span — token counts, sizes.
// Values pass the scrubber before they leave the process, like every
// other payload.
func (s *Span) SetData(key string, value any) {
	if s == nil {
		return
	}
	s.span.SetData(key, value)
}

// SetTag attaches an indexed, low-cardinality label (a provider name, a
// backend id). Never a run id or a prompt.
func (s *Span) SetTag(key, value string) {
	if s == nil {
		return
	}
	s.span.SetTag(key, value)
}

// Finish closes the span, recording err as its status. Safe on a nil
// Span, and safe to call twice.
func (s *Span) Finish(err error) {
	if s == nil {
		return
	}
	if err != nil {
		s.span.Status = sentry.SpanStatusInternalError
	} else {
		s.span.Status = sentry.SpanStatusOK
	}
	s.span.Finish()
}
