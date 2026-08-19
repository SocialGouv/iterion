package alert

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

type memTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (t *memTransport) Configure(sentry.ClientOptions) {}
func (t *memTransport) SendEvent(e *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, e)
}
func (t *memTransport) Flush(time.Duration) bool              { return true }
func (t *memTransport) FlushWithContext(context.Context) bool { return true }
func (t *memTransport) Close()                                {}

func (t *memTransport) all() []*sentry.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*sentry.Event, len(t.events))
	copy(out, t.events)
	return out
}

// With no DSN the sink does not exist, so a manager built without error
// tracking carries exactly the sinks it always carried.
func TestNewTrackerSinkNilWhenDisabled(t *testing.T) {
	if errtrack.Enabled() {
		t.Skip("errtrack already enabled by another test in this binary")
	}
	if s := NewTrackerSink(); s != nil {
		t.Fatalf("NewTrackerSink returned %T while tracking is off", s)
	}
}

func TestTrackerSinkForwardsAlerts(t *testing.T) {
	tr := &memTransport{}
	if !errtrack.Init(errtrack.Config{
		DSN:       "https://publickey@localhost/1",
		Transport: tr,
		Logger:    iterlog.New(iterlog.LevelError, io.Discard),
	}) {
		t.Fatal("errtrack.Init returned false with a valid DSN")
	}
	t.Cleanup(func() { sentry.CurrentHub().BindClient(nil) })

	sink := NewTrackerSink()
	if sink == nil {
		t.Fatal("NewTrackerSink returned nil while tracking is enabled")
	}

	sink.Notify(context.Background(), Alert{
		Kind:    KindRunFailed,
		RunID:   "run-1",
		RunName: "review-pr",
		NodeID:  "implement",
		Reason:  "EXECUTION_FAILED",
		Link:    "https://studio.example/runs/run-1",
	})
	sink.Notify(context.Background(), Alert{
		Kind:      KindBudgetWarning,
		RunID:     "run-2",
		Axis:      "cost_usd",
		BudgetPct: 82,
	})
	// A recovery is context, not an incident.
	sink.Notify(context.Background(), Alert{Kind: KindStallRecovered, RunID: "run-3"})
	errtrack.Flush()

	events := tr.all()
	if len(events) != 2 {
		t.Fatalf("want 2 events (failure + budget warning), got %d", len(events))
	}

	failed := events[0]
	if failed.Message != "run alert: run_failed" {
		t.Errorf("message = %q", failed.Message)
	}
	if failed.Level != sentry.LevelError {
		t.Errorf("level = %q, want error", failed.Level)
	}
	ctx := failed.Contexts["iterion"]
	if ctx["run_id"] != "run-1" || ctx["node_id"] != "implement" || ctx["reason"] != "EXECUTION_FAILED" {
		t.Errorf("context = %+v", ctx)
	}

	warned := events[1]
	if warned.Level != sentry.LevelWarning {
		t.Errorf("budget warning level = %q, want warning", warned.Level)
	}
	if warned.Contexts["iterion"]["axis"] != "cost_usd" {
		t.Errorf("budget context = %+v", warned.Contexts["iterion"])
	}
	// The recovery left a breadcrumb on the last event's successor —
	// what matters here is that it produced no event of its own.
}
