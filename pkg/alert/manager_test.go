package alert

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// captureSink records every alert it receives.
type captureSink struct {
	mu     sync.Mutex
	alerts []Alert
}

func (c *captureSink) Notify(_ context.Context, a Alert) {
	c.mu.Lock()
	c.alerts = append(c.alerts, a)
	c.mu.Unlock()
}

func (c *captureSink) snapshot() []Alert {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Alert, len(c.alerts))
	copy(out, c.alerts)
	return out
}

// waitFor polls until cond is true or the deadline elapses. Sinks fire
// in goroutines, so assertions must tolerate async delivery.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within deadline")
}

func newTestManager(sink Sink) *Manager {
	return NewManager(
		WithStallTimeout(5*time.Minute),
		WithBaseURL("http://localhost:4891/"),
		WithRunLookup(func(id string) (string, bool) { return "nice-run", true }),
		WithSinks(sink),
	)
}

func TestBudgetWarningFiresOncePerAxis(t *testing.T) {
	sink := &captureSink{}
	m := newTestManager(sink)

	warn := func(axis string, used, limit float64) store.Event {
		return store.Event{
			RunID: "r1", Type: store.EventBudgetWarning, NodeID: "agent",
			Timestamp: time.Now(),
			Data:      map[string]interface{}{"dimension": axis, "used": used, "limit": limit},
		}
	}
	m.Observe(warn("tokens", 80, 100))
	m.Observe(warn("tokens", 85, 100)) // same axis — must not re-fire
	m.Observe(warn("cost_usd", 0.9, 1)) // different axis — fires

	waitFor(t, func() bool { return len(sink.snapshot()) == 2 })
	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("want 2 alerts, got %d", len(got))
	}
	// Sinks fire in goroutines, so order is not guaranteed — index by axis.
	byAxis := map[string]Alert{}
	for _, a := range got {
		if a.Kind != KindBudgetWarning {
			t.Fatalf("unexpected kind: %+v", a)
		}
		byAxis[a.Axis] = a
	}
	tok, ok := byAxis["tokens"]
	if !ok {
		t.Fatalf("no tokens alert in %+v", got)
	}
	if tok.BudgetPct != 80 {
		t.Errorf("BudgetPct = %v, want 80", tok.BudgetPct)
	}
	if tok.Link != "http://localhost:4891/runs/r1" {
		t.Errorf("Link = %q", tok.Link)
	}
	if tok.RunName != "nice-run" {
		t.Errorf("RunName = %q", tok.RunName)
	}
	if tok.NodeID != "agent" {
		t.Errorf("NodeID = %q", tok.NodeID)
	}
	if _, ok := byAxis["cost_usd"]; !ok {
		t.Errorf("no cost_usd alert in %+v", got)
	}
}

func TestBudgetExceededAndRunFailed(t *testing.T) {
	sink := &captureSink{}
	m := newTestManager(sink)

	m.Observe(store.Event{RunID: "r1", Type: store.EventBudgetExceeded, NodeID: "agent",
		Timestamp: time.Now(), Data: map[string]interface{}{"dimension": "cost_usd", "used": 1.0, "limit": 1.0}})
	m.Observe(store.Event{RunID: "r1", Type: store.EventBudgetExceeded,
		Timestamp: time.Now(), Data: map[string]interface{}{"dimension": "cost_usd", "used": 1.1, "limit": 1.0}}) // dedup
	m.Observe(store.Event{RunID: "r1", Type: store.EventRunFailed, NodeID: "agent",
		Timestamp: time.Now(), Data: map[string]interface{}{"error": "boom"}})

	waitFor(t, func() bool { return len(sink.snapshot()) == 2 })
	got := sink.snapshot()
	kinds := map[Kind]int{}
	for _, a := range got {
		kinds[a.Kind]++
	}
	if kinds[KindBudgetExceeded] != 1 {
		t.Errorf("budget_exceeded fired %d times, want 1", kinds[KindBudgetExceeded])
	}
	if kinds[KindRunFailed] != 1 {
		t.Errorf("run_failed fired %d times, want 1", kinds[KindRunFailed])
	}
	for _, a := range got {
		if a.Kind == KindRunFailed && a.Reason != "boom" {
			t.Errorf("run_failed reason = %q, want boom", a.Reason)
		}
	}
}

func TestStallFiresOnceAndReArms(t *testing.T) {
	sink := &captureSink{}
	base := time.Now()
	clock := base
	m := NewManager(
		WithStallTimeout(5*time.Minute),
		WithSinks(sink),
	)
	m.now = func() time.Time { return clock }

	// Run starts on a node.
	m.Observe(store.Event{RunID: "r1", Type: store.EventNodeStarted, NodeID: "agent", Timestamp: base})

	// 3 minutes pass — not yet stalled.
	clock = base.Add(3 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 0 {
		t.Fatalf("premature stall: %+v", fired)
	}

	// 6 minutes since last progress — stall fires.
	clock = base.Add(6 * time.Minute)
	fired := m.checkStalls(clock)
	if len(fired) != 1 || fired[0].Kind != KindStall {
		t.Fatalf("want 1 stall, got %+v", fired)
	}
	if fired[0].NodeID != "agent" {
		t.Errorf("stall NodeID = %q, want agent", fired[0].NodeID)
	}

	// Still stalled — must not re-fire.
	clock = base.Add(7 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 0 {
		t.Fatalf("duplicate stall: %+v", fired)
	}

	// Progress resumes (e.g. a tool event), then stalls again — re-fires.
	progressAt := base.Add(8 * time.Minute)
	m.Observe(store.Event{RunID: "r1", Type: store.EventToolCalled, NodeID: "agent", Timestamp: progressAt})
	clock = progressAt.Add(6 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 1 {
		t.Fatalf("want re-armed stall, got %+v", fired)
	}
}

func TestNoStallWhilePausedForHuman(t *testing.T) {
	sink := &captureSink{}
	base := time.Now()
	clock := base
	m := NewManager(WithStallTimeout(5*time.Minute), WithSinks(sink))
	m.now = func() time.Time { return clock }

	// Run starts, a human node requests input, the run pauses.
	m.Observe(store.Event{RunID: "r1", Type: store.EventNodeStarted, NodeID: "ask", Timestamp: base})
	m.Observe(store.Event{RunID: "r1", Type: store.EventHumanInputRequested, NodeID: "ask", Timestamp: base.Add(time.Second)})
	m.Observe(store.Event{RunID: "r1", Type: store.EventRunPaused, NodeID: "ask", Timestamp: base.Add(2 * time.Second)})

	// 30 minutes waiting for the human — a paused run is NOT stalled.
	clock = base.Add(30 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 0 {
		t.Fatalf("paused-for-human run flagged as stalled: %+v", fired)
	}

	// Human submits and the run resumes; stall detection re-arms from the
	// resume moment (not the stale pause timestamp), so it doesn't fire
	// immediately.
	resumeAt := base.Add(30 * time.Minute)
	m.Observe(store.Event{RunID: "r1", Type: store.EventHumanAnswersRecorded, NodeID: "ask", Timestamp: resumeAt})
	m.Observe(store.Event{RunID: "r1", Type: store.EventRunResumed, NodeID: "ask", Timestamp: resumeAt})
	clock = resumeAt.Add(3 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 0 {
		t.Fatalf("stall fired too soon after resume: %+v", fired)
	}

	// If the resumed run then genuinely goes silent past the timeout, a
	// real stall DOES fire — the suppression was scoped to the pause.
	clock = resumeAt.Add(6 * time.Minute)
	fired := m.checkStalls(clock)
	if len(fired) != 1 || fired[0].Kind != KindStall {
		t.Fatalf("post-resume genuine stall did not fire: %+v", fired)
	}
}

func TestNoStallWhileToolEventsFlow(t *testing.T) {
	sink := &captureSink{}
	base := time.Now()
	clock := base
	m := NewManager(WithStallTimeout(5*time.Minute), WithSinks(sink))
	m.now = func() time.Time { return clock }

	m.Observe(store.Event{RunID: "r1", Type: store.EventNodeStarted, NodeID: "agent", Timestamp: base})
	// A long node that emits a tool event every 2 minutes for 20 minutes
	// must never be flagged as stalled.
	for i := 1; i <= 10; i++ {
		at := base.Add(time.Duration(i) * 2 * time.Minute)
		m.Observe(store.Event{RunID: "r1", Type: store.EventToolCalled, NodeID: "agent", Timestamp: at})
		clock = at.Add(time.Second)
		if fired := m.checkStalls(clock); len(fired) != 0 {
			t.Fatalf("false stall at tick %d: %+v", i, fired)
		}
	}
}

func TestTerminalRunDoesNotStall(t *testing.T) {
	sink := &captureSink{}
	base := time.Now()
	clock := base
	m := NewManager(WithStallTimeout(5*time.Minute), WithSinks(sink))
	m.now = func() time.Time { return clock }

	m.Observe(store.Event{RunID: "r1", Type: store.EventNodeStarted, NodeID: "agent", Timestamp: base})
	m.Observe(store.Event{RunID: "r1", Type: store.EventRunFinished, Timestamp: base.Add(time.Minute)})

	clock = base.Add(30 * time.Minute)
	if fired := m.checkStalls(clock); len(fired) != 0 {
		t.Fatalf("terminal run flagged stalled: %+v", fired)
	}
}

func TestStartStopTicker(t *testing.T) {
	m := NewManager(WithStallTimeout(20 * time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	m.Stop() // must not panic / double-close
	m.Stop()
}
