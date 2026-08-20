package errtrack

import (
	"context"
	"errors"
	"testing"

	sentry "github.com/getsentry/sentry-go"
)

func TestStartSpanIsFreeWhenTracingIsOff(t *testing.T) {
	t.Setenv(EnvTracesSampleRate, "")
	tr := enable(t, Config{})

	base := context.Background()
	ctx, span := StartSpan(base, "llm.generate", "anthropic/claude-opus-5")
	if span != nil {
		t.Fatal("a span was allocated while tracing is off")
	}
	//nolint:staticcheck // the point IS that the caller's own ctx comes back.
	if ctx != base {
		t.Fatal("StartSpan replaced the caller's context while tracing is off")
	}
	// Every method has to tolerate the nil handle, or the call site
	// needs an `if` around it and the seam stops being free.
	span.SetTag("llm.provider", "anthropic")
	span.SetData("llm.input_tokens", 42)
	span.Finish(errors.New("boom"))
	span.Finish(nil)

	Flush()
	if got := len(transactions(tr.all())); got != 0 {
		t.Fatalf("tracing off produced %d transaction(s)", got)
	}
}

func TestStartSpanWithNoParentIsAStandaloneTransaction(t *testing.T) {
	tr := enable(t, Config{TracesSampleRate: rate(1)})

	_, span := StartSpan(context.Background(), "llm.generate", "anthropic/claude-opus-5")
	span.SetTag("llm.provider", "anthropic")
	span.SetData("llm.input_tokens", 1200)
	span.Finish(nil)
	Flush()

	txns := transactions(tr.all())
	if len(txns) != 1 {
		t.Fatalf("want 1 transaction, got %d", len(txns))
	}
	if txns[0].Transaction != "llm.generate anthropic/claude-opus-5" {
		t.Errorf("transaction name = %q", txns[0].Transaction)
	}
	if txns[0].Tags["llm.provider"] != "anthropic" {
		t.Errorf("tags = %v", txns[0].Tags)
	}
}

func TestStartSpanUnderARequestIsAChildSpan(t *testing.T) {
	tr := enable(t, Config{TracesSampleRate: rate(1)})

	txn := sentry.StartTransaction(context.Background(), "GET /api/runs/{id}")
	ctx, span := StartSpan(txn.Context(), "llm.generate", "openai/gpt-5.4-mini")
	span.SetData("llm.output_tokens", 7)
	span.Finish(errors.New("stream died"))
	txn.Finish()
	Flush()

	txns := transactions(tr.all())
	if len(txns) != 1 {
		t.Fatalf("want 1 transaction (the request), got %d", len(txns))
	}
	if len(txns[0].Spans) != 1 {
		t.Fatalf("the generation did not attach to the request: %d span(s)", len(txns[0].Spans))
	}
	child := txns[0].Spans[0]
	if child.Op != "llm.generate" {
		t.Errorf("span op = %q", child.Op)
	}
	if child.Description != "openai/gpt-5.4-mini" {
		t.Errorf("span description = %q", child.Description)
	}
	if child.Status != sentry.SpanStatusInternalError {
		t.Errorf("a failed call reported status %v", child.Status)
	}
	if sentry.SpanFromContext(ctx) == nil {
		t.Error("StartSpan returned a context carrying no span")
	}
}
