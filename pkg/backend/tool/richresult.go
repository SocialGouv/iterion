package tool

import (
	"bytes"
	"context"
	"sync"
)

// RichResultSink receives the structured content of ONE nested tool call.
//
// MCP servers report a result as the text the model reads plus an optional
// structuredContent; the registration closure in pkg/backend/mcp flattens the
// pair to text, which is the model-facing contract that pre-dates CM-7. The
// script surface gained the structured half in CM-7: when a call runs inside
// a codemode script, the registration closure observes the result into the
// sink the script tool server armed on the call's context, and that server
// reads the sink back into the outcome the script receives.
//
// It is armed per call, a fresh sink per CallTool, so two calls of one batch
// never share one. The mutex keeps the type safe for any reader and keeps
// the race detector quiet on any interleaving.
type RichResultSink struct {
	mu         sync.Mutex
	structured []byte
	observed   bool
}

// NewRichResultSink returns an empty sink.
func NewRichResultSink() *RichResultSink {
	return &RichResultSink{}
}

// Observe records raw as the call's structured content, REPLACING any earlier
// observation: the Read auto-retry re-calls the tool and the retry's result
// is the program's answer. raw is copied, so a caller may pass and reuse its
// buffer. A nil sink and nil raw are no-ops, so the closure can observe
// unconditionally.
func (s *RichResultSink) Observe(raw []byte) {
	if s == nil || raw == nil {
		return
	}
	cp := bytes.Clone(raw)
	s.mu.Lock()
	s.structured = cp
	s.observed = true
	s.mu.Unlock()
}

// Result reports the last observation: the payload and whether one was made.
// The bytes are sink-internal and read-only for the caller; the only intended
// consumer is the script tool server, which reads them straight into the
// ToolOutcome it returns.
func (s *RichResultSink) Result() ([]byte, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.structured, s.observed
}

// ---------------------------------------------------------------------------
// Context accessors
// ---------------------------------------------------------------------------

// ctxKeyRichResultSink carries the call's rich-result sink.
type ctxKeyRichResultSink struct{}

// WithRichResultSink arms sink on the context a nested tool's Execute
// receives. The writer of the value is the script tool server; the reader is
// the MCP registration closure.
func WithRichResultSink(ctx context.Context, sink *RichResultSink) context.Context {
	return context.WithValue(ctx, ctxKeyRichResultSink{}, sink)
}

// RichResultSinkFrom reads the call's sink. The second return is false when
// the call does not run inside a codemode script, which is the direct path's
// answer: no sink, no observation, and the flattening text contract carries
// on exactly as before CM-7.
func RichResultSinkFrom(ctx context.Context) (*RichResultSink, bool) {
	sink, ok := ctx.Value(ctxKeyRichResultSink{}).(*RichResultSink)
	return sink, ok && sink != nil
}
