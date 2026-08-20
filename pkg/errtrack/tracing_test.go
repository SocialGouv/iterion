package errtrack

import (
	"bytes"
	"context"
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
