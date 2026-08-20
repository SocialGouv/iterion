package errtrack

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/log"
)

func rate(v float64) *float64 { return &v }

// transactions filters the recorded envelopes down to transaction
// events — the only ones tracing produces.
func transactions(evs []*sentry.Event) []*sentry.Event {
	var out []*sentry.Event
	for _, e := range evs {
		if e.Type == "transaction" {
			out = append(out, e)
		}
	}
	return out
}

func TestTracingIsOffWhenTheSampleRateEnvIsUnset(t *testing.T) {
	t.Setenv(EnvTracesSampleRate, "")

	var buf bytes.Buffer
	tr := enable(t, Config{Logger: log.New(log.LevelTrace, &buf)})

	if !Enabled() {
		t.Fatal("error tracking should be on with a DSN")
	}
	if TracingEnabled() {
		t.Fatal("tracing must stay off when the sample rate is unset")
	}

	// The whole point: a transaction started anyway is never recorded.
	sentry.StartTransaction(context.Background(), "unsampled").Finish()
	Flush()
	if got := len(transactions(tr.all())); got != 0 {
		t.Fatalf("tracing off produced %d transaction(s)", got)
	}
}

func TestTracingOnRecordsOneTransactionPerUnitOfWork(t *testing.T) {
	tr := enable(t, Config{TracesSampleRate: rate(1)})

	if !TracingEnabled() {
		t.Fatal("TracingEnabled() false with rate 1")
	}
	sentry.StartTransaction(context.Background(), "iterion.test.unit").Finish()
	Flush()

	txns := transactions(tr.all())
	if len(txns) != 1 {
		t.Fatalf("want 1 transaction, got %d", len(txns))
	}
	if txns[0].Transaction != "iterion.test.unit" {
		t.Errorf("transaction name = %q", txns[0].Transaction)
	}
}

func TestExplicitZeroSampleRateIsOff(t *testing.T) {
	// The env says "trace everything"; the caller's explicit 0 wins.
	t.Setenv(EnvTracesSampleRate, "1.0")
	tr := enable(t, Config{TracesSampleRate: rate(0)})

	if TracingEnabled() {
		t.Fatal("an explicit rate of 0 must disable tracing")
	}
	sentry.StartTransaction(context.Background(), "unsampled").Finish()
	Flush()
	if got := len(transactions(tr.all())); got != 0 {
		t.Fatalf("rate 0 produced %d transaction(s)", got)
	}
}

func TestSampleRateFromTheEnvEnablesTracing(t *testing.T) {
	t.Setenv(EnvTracesSampleRate, "1")
	tr := enable(t, Config{})

	if !TracingEnabled() {
		t.Fatal("SENTRY_TRACES_SAMPLE_RATE=1 did not enable tracing")
	}
	sentry.StartTransaction(context.Background(), "from.env").Finish()
	Flush()
	if got := len(transactions(tr.all())); got != 1 {
		t.Fatalf("want 1 transaction, got %d", got)
	}
}

func TestUnusableSampleRateIsLoudAndOff(t *testing.T) {
	for _, raw := range []string{"banana", "1.5", "-0.1", "NaN", "Inf"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(EnvTracesSampleRate, raw)
			var buf bytes.Buffer
			enable(t, Config{Logger: log.New(log.LevelError, &buf)})

			if TracingEnabled() {
				t.Fatalf("%q enabled tracing", raw)
			}
			if !Enabled() {
				t.Fatal("a bad sample rate must not disable ERROR tracking")
			}
			out := buf.String()
			if !strings.Contains(out, EnvTracesSampleRate) {
				t.Fatalf("the refusal was not reported: %q", out)
			}
		})
	}
}

func TestABadSampleRateIsSilentWhenTrackingIsOff(t *testing.T) {
	t.Setenv(EnvDSN, "")
	t.Setenv(EnvTracesSampleRate, "banana")
	reset()
	t.Cleanup(reset)

	var buf bytes.Buffer
	if Init(Config{Logger: log.New(log.LevelTrace, &buf)}) {
		t.Fatal("Init reported enabled with no DSN")
	}
	if got := buf.String(); got != "" {
		t.Fatalf("no DSN must mean no output at all, got %q", got)
	}
}

func TestSpansAreScrubbedBeforeSend(t *testing.T) {
	tr := enable(t, Config{TracesSampleRate: rate(1)})

	txn := sentry.StartTransaction(context.Background(), "iterion.test.scrub")
	span := txn.StartChild("llm.generate")
	span.Description = "call to https://user:hunter2@api.example.com/v1"
	span.SetTag("api_key", "sk-live-0123456789abcdef")
	span.SetData("operator", "someone@example.com")
	span.SetData("model", "anthropic/claude-opus-5")
	span.Finish()
	txn.Finish()
	Flush()

	txns := transactions(tr.all())
	if len(txns) != 1 {
		t.Fatalf("want 1 transaction, got %d", len(txns))
	}
	if len(txns[0].Spans) != 1 {
		t.Fatalf("want 1 child span, got %d", len(txns[0].Spans))
	}
	got := txns[0].Spans[0]
	if strings.Contains(got.Description, "hunter2") {
		t.Errorf("span description leaked userinfo: %q", got.Description)
	}
	if got.Tags["api_key"] != redacted {
		t.Errorf("span tag api_key = %q", got.Tags["api_key"])
	}
	if s, _ := got.Data["operator"].(string); strings.Contains(s, "someone@example.com") {
		t.Errorf("span data leaked an email: %q", s)
	}
	if got.Data["model"] != "anthropic/claude-opus-5" {
		t.Errorf("scrubbing destroyed a harmless span field: %v", got.Data["model"])
	}
}

// A context that still carries a FINISHED transaction — exactly what a
// run launched from an API request holds by the time it makes its first
// LLM call — silently orphans any child started on it: the SDK drops a
// child finished after its transaction was sent. This test pins that
// SDK behaviour (the reason StartIndependent exists) and proves the
// independent form survives the same context.
func TestIndependentSpanSurvivesAFinishedParentTransaction(t *testing.T) {
	tr := enable(t, Config{TracesSampleRate: rate(1)})

	parent := sentry.StartSpan(context.Background(), "http.server",
		sentry.WithTransactionName("GET /api/runs"))
	deadCtx := parent.Context()
	parent.Finish()
	Flush()
	if got := len(transactions(tr.all())); got != 1 {
		t.Fatalf("expected the request transaction alone, got %d", got)
	}

	// The orphan: a child of the finished transaction is never exported.
	_, orphan := StartSpan(deadCtx, "llm.generate", "anthropic/claude-opus-5")
	orphan.Finish(nil)
	Flush()
	if got := len(transactions(tr.all())); got != 1 {
		t.Fatalf("a child of a finished transaction should be dropped by the SDK; transport now has %d", got)
	}

	// The fix: the independent form exports regardless of the context.
	span := StartIndependent("llm.generate", "anthropic/claude-opus-5")
	span.SetTag("llm.provider", "anthropic")
	span.Finish(nil)
	Flush()
	if got := len(transactions(tr.all())); got != 2 {
		t.Fatalf("StartIndependent should export its own transaction; transport has %d", got)
	}
}

// StartIndependent must not leak its transaction onto the process-global
// hub scope: sentry.StartSpan installs the span on the hub's scope and,
// for a transaction, doFinish never restores what was there — so on the
// global hub every LATER error event would inherit the trace context of
// a stale (or, under parallel generations, arbitrary) llm.generate
// transaction. The independent span therefore rides a CLONED hub.
func TestIndependentSpanDoesNotContaminateGlobalErrorTraceContext(t *testing.T) {
	tr := enable(t, Config{TracesSampleRate: rate(1)})

	span := StartIndependent("llm.generate", "anthropic/claude-opus-5")
	span.Finish(nil)
	Flush()
	txs := transactions(tr.all())
	if len(txs) != 1 {
		t.Fatalf("expected the llm transaction, got %d", len(txs))
	}
	llmTrace := txs[0].Contexts["trace"]["trace_id"]

	CaptureError(errors.New("boom after generation"), nil)
	Flush()
	var errEvent *sentry.Event
	for _, e := range tr.all() {
		if e.Type != "transaction" {
			errEvent = e
		}
	}
	if errEvent == nil {
		t.Fatal("no error event captured")
	}
	if tc, ok := errEvent.Contexts["trace"]; ok {
		if tc["trace_id"] == llmTrace {
			t.Fatalf("error event inherited the finished llm transaction's trace context (%v) — StartIndependent leaked its span onto the global hub scope", llmTrace)
		}
	}
}
