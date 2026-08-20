package model

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// spanTransport records events in-process; nothing reaches a network.
type spanTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (t *spanTransport) Configure(sentry.ClientOptions) {}
func (t *spanTransport) SendEvent(e *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, e)
}
func (t *spanTransport) Flush(time.Duration) bool              { return true }
func (t *spanTransport) FlushWithContext(context.Context) bool { return true }
func (t *spanTransport) Close()                                {}

func (t *spanTransport) transactions() []*sentry.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*sentry.Event
	for _, e := range t.events {
		if e.Type == "transaction" {
			out = append(out, e)
		}
	}
	return out
}

// errtrack installs its client once per PROCESS, so this is the single
// transport of the binary and the single Init.
var (
	genTransport   = &spanTransport{}
	genTransportOn sync.Once
)

func enableGenerationTracing(t *testing.T) *spanTransport {
	t.Helper()
	genTransportOn.Do(func() {
		full := 1.0
		errtrack.Init(errtrack.Config{
			DSN:              "https://publickey@localhost/1",
			Transport:        genTransport,
			Logger:           iterlog.New(iterlog.LevelError, io.Discard),
			TracesSampleRate: &full,
		})
	})
	if !errtrack.TracingEnabled() {
		t.Fatal("errtrack.Init did not enable tracing")
	}
	return genTransport
}

// The off-state proof for the generation seam — the shape every
// deployment that has not set SENTRY_TRACES_SAMPLE_RATE runs in.
// errtrack's Init is once-per-process, so when another test in the
// package (in whatever order -shuffle picked) has already enabled
// tracing, the off state is simply not observable here any more: SKIP
// rather than fail — pkg/errtrack's own tests (which have a test-only
// reset) keep the off-state covered regardless of order.
func TestGenerationEmitsNoTransactionWhenTracingIsOff(t *testing.T) {
	if errtrack.TracingEnabled() {
		t.Skip("tracing already enabled by another test in this process; off-state covered by pkg/errtrack's own tests")
	}
	client := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("hi", "end_turn")}}

	agg, err := callAndAggregate(context.Background(), client, api.CreateMessageRequest{},
		GenerationOptions{Model: "anthropic/claude-opus-5"})
	if err != nil {
		t.Fatalf("callAndAggregate: %v", err)
	}
	if agg.text != "hi" {
		t.Fatalf("text = %q — instrumentation changed the result", agg.text)
	}
	if got := len(genTransport.transactions()); got != 0 {
		t.Fatalf("tracing off produced %d transaction(s)", got)
	}
}

func TestGenerationIsTracedAsOneTaggedTransaction(t *testing.T) {
	tr := enableGenerationTracing(t)
	before := len(tr.transactions())

	client := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("hello", "end_turn")}}
	if _, err := callAndAggregate(context.Background(), client, api.CreateMessageRequest{},
		GenerationOptions{Model: "anthropic/claude-opus-5"}); err != nil {
		t.Fatalf("callAndAggregate: %v", err)
	}
	errtrack.Flush()

	txns := tr.transactions()[before:]
	if len(txns) != 1 {
		t.Fatalf("want 1 transaction for 1 provider call, got %d", len(txns))
	}
	txn := txns[0]
	if txn.Transaction != "llm.generate anthropic/claude-opus-5" {
		t.Errorf("transaction name = %q", txn.Transaction)
	}
	if txn.Tags["llm.provider"] != "anthropic" {
		t.Errorf("llm.provider tag = %q", txn.Tags["llm.provider"])
	}
	if txn.Tags["llm.model"] != "claude-opus-5" {
		t.Errorf("llm.model tag = %q", txn.Tags["llm.model"])
	}
	// The stream fixture bills 100 in / 50 out.
	if txn.Contexts["trace"]["data"] == nil {
		t.Fatalf("the transaction carries no span data: %+v", txn.Contexts["trace"])
	}
	data, _ := txn.Contexts["trace"]["data"].(map[string]any)
	if data["llm.input_tokens"] != 100 || data["llm.output_tokens"] != 50 {
		t.Errorf("token measurements = %v", data)
	}
}

// A provider that never opens the stream must still close its span, and
// say it failed.
func TestAFailedProviderCallIsTracedAsAnError(t *testing.T) {
	tr := enableGenerationTracing(t)
	before := len(tr.transactions())

	client := &execMockClient{err: errors.New("provider unreachable")}
	if _, err := callAndAggregate(context.Background(), client, api.CreateMessageRequest{},
		GenerationOptions{Model: "openai/gpt-5.4-mini"}); err == nil {
		t.Fatal("want the provider error to propagate")
	}
	errtrack.Flush()

	txns := tr.transactions()[before:]
	if len(txns) != 1 {
		t.Fatalf("want 1 transaction, got %d", len(txns))
	}
	if got, _ := txns[0].Contexts["trace"]["status"].(string); got != "internal_error" {
		t.Errorf("trace status = %q, want internal_error", got)
	}
}

func TestModelProviderSplitsTheRoutingPrefix(t *testing.T) {
	for spec, want := range map[string]string{
		"anthropic/claude-opus-5": "anthropic",
		"openai/gpt-5.4-mini":     "openai",
		"claude-opus-4-8":         "default",
		"":                        "default",
	} {
		if got := modelProvider(spec); got != want {
			t.Errorf("modelProvider(%q) = %q, want %q", spec, got, want)
		}
	}
}
