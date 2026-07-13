package supervise

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// fakeObserver feeds a hand-built event channel.
type fakeObserver struct{ ch chan *store.Event }

func (f *fakeObserver) ObserveRun(ctx context.Context, runID string) (<-chan *store.Event, func(), error) {
	return f.ch, func() {}, nil
}

// recordInjector records every injected (nodeID, text) so a test can
// assert node-scoping and content.
type recordInjector struct {
	mu   sync.Mutex
	msgs []injected
}

type injected struct{ node, text string }

func (r *recordInjector) Inject(ctx context.Context, runID, nodeID, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, injected{node: nodeID, text: text})
	return nil
}

func (r *recordInjector) snapshot() []injected {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]injected(nil), r.msgs...)
}

// stubEval intervenes only on monitor-driven wakes, returning a fixed
// steering message; turn-boundary wakes are no-ops.
type stubEval struct {
	mu    sync.Mutex
	calls int
}

func (s *stubEval) Evaluate(ctx context.Context, in EvalInput) (*Decision, EvalUsage, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if strings.HasPrefix(in.WakeReason, "monitor") {
		return &Decision{Intervene: true, Message: "re-read the import error and fix it", Reason: "bash test failed"}, EvalUsage{InputTokens: 10, OutputTokens: 5}, nil
	}
	return &Decision{Intervene: false}, EvalUsage{}, nil
}

func ev(seq int64, typ store.EventType, node string, data map[string]any) *store.Event {
	return &store.Event{Seq: seq, Type: typ, NodeID: node, Data: data, Timestamp: time.Now()}
}

// A supervisor watching node "implement" with a tool_error monitor must:
// arm on node_started, inject a node-scoped message when a matching
// tool_error fires while armed, and stay silent once the node finished
// (disarmed) even if another tool_error arrives.
func TestCoordinatorNodeScopedMonitorInjection(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 16)}
	inj := &recordInjector{}
	spec := Spec{
		Name:     "watchdog",
		Watches:  []string{"implement"},
		Monitors: []Monitor{{EventType: "tool_error", ToolName: "Bash"}},
		Cooldown: time.Millisecond,
		MaxEvals: 10,
	}
	c := New(obs, inj, "r1", spec, &stubEval{}, nil)
	if c == nil {
		t.Fatal("New returned nil")
	}
	c.Start(context.Background())
	defer c.Close()

	// Arm on the watched node, then a matching Bash tool_error fires.
	obs.ch <- ev(1, store.EventNodeStarted, "implement", nil)
	obs.ch <- ev(2, store.EventToolError, "implement", map[string]any{"tool": "Bash", "error": "cannot find package"})

	waitFor(t, func() bool { return len(inj.snapshot()) == 1 })
	got := inj.snapshot()
	if got[0].node != "implement" {
		t.Fatalf("injected node=%q; want implement (node-scoped)", got[0].node)
	}
	if !strings.Contains(got[0].text, "fix it") {
		t.Fatalf("injected text=%q; want the steering message", got[0].text)
	}

	// Node finishes → supervisor disarms. A later tool_error must NOT
	// trigger another injection.
	obs.ch <- ev(3, store.EventNodeFinished, "implement", nil)
	obs.ch <- ev(4, store.EventToolError, "implement", map[string]any{"tool": "Bash", "error": "again"})
	// Give the worker time to (not) act.
	time.Sleep(150 * time.Millisecond)
	if n := len(inj.snapshot()); n != 1 {
		t.Fatalf("after disarm got %d injections; want exactly 1", n)
	}
}

// A monitor for a different tool must not fire on an unrelated tool_error.
func TestCoordinatorMonitorToolFilter(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 16)}
	inj := &recordInjector{}
	spec := Spec{
		Name:     "wd",
		Watches:  []string{"implement"},
		Monitors: []Monitor{{EventType: "tool_error", ToolName: "Bash"}},
		Cooldown: time.Millisecond,
		MaxEvals: 10,
	}
	c := New(obs, inj, "r1", spec, &stubEval{}, nil)
	c.Start(context.Background())
	defer c.Close()

	obs.ch <- ev(1, store.EventNodeStarted, "implement", nil)
	// An Edit tool error — monitor is scoped to Bash, must not fire.
	obs.ch <- ev(2, store.EventToolError, "implement", map[string]any{"tool": "Edit", "error": "no such file"})
	time.Sleep(150 * time.Millisecond)
	if n := len(inj.snapshot()); n != 0 {
		t.Fatalf("Edit tool_error wrongly fired Bash monitor: %d injections", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
