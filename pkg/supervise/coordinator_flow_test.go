package supervise

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// scriptedEval returns pre-scripted decisions in order, then repeats the
// last one. It records every EvalInput it saw.
type scriptedEval struct {
	mu        sync.Mutex
	decisions []*Decision
	inputs    []EvalInput
}

func (s *scriptedEval) Evaluate(ctx context.Context, in EvalInput) (*Decision, EvalUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputs = append(s.inputs, in)
	idx := len(s.inputs) - 1
	if idx >= len(s.decisions) {
		idx = len(s.decisions) - 1
	}
	return s.decisions[idx], EvalUsage{InputTokens: 1, OutputTokens: 1}, nil
}

func (s *scriptedEval) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inputs)
}

func TestNewNilPrerequisites(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event)}
	inj := &recordInjector{}
	if New(nil, inj, "r1", Spec{}, &stubEval{}, nil) != nil {
		t.Error("nil observer should yield nil coordinator")
	}
	if New(obs, nil, "r1", Spec{}, &stubEval{}, nil) != nil {
		t.Error("nil injector should yield nil coordinator")
	}
	if New(obs, inj, "", Spec{}, &stubEval{}, nil) != nil {
		t.Error("empty run id should yield nil coordinator")
	}
	// nil-coordinator methods are safe no-ops.
	var c *Coordinator
	c.Start(context.Background())
	c.Close()
}

func TestNewAppliesSpecDefaults(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event)}
	c := New(obs, &recordInjector{}, "r1", Spec{}, &stubEval{}, nil)
	if c.spec.Cooldown != DefaultCooldown {
		t.Errorf("Cooldown = %v; want default %v", c.spec.Cooldown, DefaultCooldown)
	}
	if c.spec.MaxEvals != DefaultMaxEvals {
		t.Errorf("MaxEvals = %d; want default %d", c.spec.MaxEvals, DefaultMaxEvals)
	}
}

// --- unit tests on an unstarted coordinator (single-goroutine access) ---

func newBareCoordinator(t *testing.T, spec Spec, eval Evaluator, inj Injector) *Coordinator {
	t.Helper()
	if inj == nil {
		inj = &recordInjector{}
	}
	c := New(&fakeObserver{ch: make(chan *store.Event)}, inj, "r1", spec, eval, nil)
	if c == nil {
		t.Fatal("New returned nil")
	}
	return c
}

func TestEvaluateCooldownAndBypass(t *testing.T) {
	eval := &scriptedEval{decisions: []*Decision{{Intervene: false}}}
	c := newBareCoordinator(t, Spec{Cooldown: time.Hour, MaxEvals: 10}, eval, nil)

	c.evaluate("turn_boundary", false)
	if eval.calls() != 1 {
		t.Fatalf("first evaluation suppressed: calls=%d", eval.calls())
	}
	// Within the cooldown, a plain turn-boundary wake is suppressed…
	c.evaluate("turn_boundary", false)
	if eval.calls() != 1 {
		t.Fatalf("cooldown not honoured: calls=%d", eval.calls())
	}
	// …but a monitor wake bypasses it.
	c.evaluate("monitor matched: x", true)
	if eval.calls() != 2 {
		t.Fatalf("bypassCooldown not honoured: calls=%d", eval.calls())
	}
}

func TestEvaluateMaxEvalsBudget(t *testing.T) {
	eval := &scriptedEval{decisions: []*Decision{{Intervene: false}}}
	c := newBareCoordinator(t, Spec{Cooldown: time.Nanosecond, MaxEvals: 2}, eval, nil)

	for i := 0; i < 5; i++ {
		c.evaluate(fmt.Sprintf("monitor %d", i), true)
	}
	if eval.calls() != 2 {
		t.Fatalf("budget: evaluator called %d times; want exactly 2", eval.calls())
	}
	// Current behavior: the exhaustion log bumps the counter once more
	// (log-once marker), so evalCount lands at MaxEvals+1.
	if c.evalCount != 3 {
		t.Errorf("evalCount = %d; want 3 (MaxEvals + log-once marker)", c.evalCount)
	}
	if c.inTokens != 2 || c.outTokens != 2 {
		t.Errorf("token accounting = (%d, %d); want (2, 2)", c.inTokens, c.outTokens)
	}
}

func TestEvaluateSkippedWhenFinished(t *testing.T) {
	eval := &scriptedEval{decisions: []*Decision{{Intervene: false}}}
	c := newBareCoordinator(t, Spec{MaxEvals: 10}, eval, nil)
	c.finished = true
	c.evaluate("monitor matched: x", true)
	if eval.calls() != 0 {
		t.Fatalf("finished coordinator still evaluated: calls=%d", eval.calls())
	}
}

func TestApplyDecision(t *testing.T) {
	t.Run("nil decision is a no-op", func(t *testing.T) {
		c := newBareCoordinator(t, Spec{}, &stubEval{}, nil)
		c.applyDecision(nil)
	})

	t.Run("empty watch monitors are filtered", func(t *testing.T) {
		c := newBareCoordinator(t, Spec{}, &stubEval{}, nil)
		c.applyDecision(&Decision{Watch: []Monitor{{}, {EventType: "tool_error"}, {}}})
		if len(c.monitors) != 1 || c.monitors[0].EventType != "tool_error" {
			t.Fatalf("monitors = %+v; want the single non-empty one", c.monitors)
		}
		// A registered monitor participates in matching, flagged as
		// bot-registered (cooldown-bypassing class).
		if matched, registered := c.matchesMonitor(ev(1, store.EventToolError, "n", nil)); !matched || !registered {
			t.Errorf("registered watch monitor: matched=%v registered=%v; want true/true", matched, registered)
		}
	})

	t.Run("intervene with blank message does not inject", func(t *testing.T) {
		inj := &recordInjector{}
		c := newBareCoordinator(t, Spec{}, &stubEval{}, inj)
		c.applyDecision(&Decision{Intervene: true, Message: "   "})
		if n := len(inj.snapshot()); n != 0 {
			t.Fatalf("blank message injected %d times; want 0", n)
		}
	})

	t.Run("done marks finished", func(t *testing.T) {
		c := newBareCoordinator(t, Spec{}, &stubEval{}, nil)
		c.applyDecision(&Decision{Done: true})
		if !c.finished {
			t.Error("Done decision did not set finished")
		}
	})
}

func TestInjectScopingAndFraming(t *testing.T) {
	t.Run("node-scoped with supervisor name framing", func(t *testing.T) {
		inj := &recordInjector{}
		c := newBareCoordinator(t, Spec{Name: "wd", Watches: []string{"impl"}}, &stubEval{}, inj)
		c.lastWatchedActive = "impl"
		c.inject("fix the test")
		got := inj.snapshot()
		if len(got) != 1 {
			t.Fatalf("injections = %d; want 1", len(got))
		}
		if got[0].node != "impl" {
			t.Errorf("node = %q; want impl", got[0].node)
		}
		if got[0].text != "[supervisor wd] fix the test" {
			t.Errorf("text = %q; want framed message", got[0].text)
		}
	})

	t.Run("whole-run supervisor injects run-scoped without framing", func(t *testing.T) {
		inj := &recordInjector{}
		c := newBareCoordinator(t, Spec{}, &stubEval{}, inj)
		c.lastWatchedActive = "impl" // ignored for run scope
		c.inject("go on")
		got := inj.snapshot()
		if len(got) != 1 || got[0].node != "" {
			t.Fatalf("injected = %+v; want one run-scoped message", got)
		}
		if got[0].text != "go on" {
			t.Errorf("text = %q; empty Name must not add framing", got[0].text)
		}
	})
}

func TestIngest(t *testing.T) {
	t.Run("active node tracking", func(t *testing.T) {
		c := newBareCoordinator(t, Spec{Watches: []string{"a"}}, &stubEval{}, nil)
		c.ingest(ev(1, store.EventNodeStarted, "a", nil))
		if c.lastWatchedActive != "a" {
			t.Fatalf("lastWatchedActive = %q; want a", c.lastWatchedActive)
		}
		if !c.armed() {
			t.Error("watched active node should arm")
		}
		// Finishing a DIFFERENT node does not clear the watched node.
		c.ingest(ev(2, store.EventNodeFinished, "b", nil))
		if c.lastWatchedActive != "a" {
			t.Errorf("lastWatchedActive = %q after unrelated node_finished; want a", c.lastWatchedActive)
		}
		c.ingest(ev(3, store.EventNodeFinished, "a", nil))
		if c.lastWatchedActive != "" {
			t.Errorf("lastWatchedActive = %q after node_finished; want empty", c.lastWatchedActive)
		}
		if c.armed() {
			t.Error("disarmed after watched node finished")
		}
	})

	t.Run("concurrent sibling does not disarm the watched node", func(t *testing.T) {
		// A single-slot "active node" was permanently cleared by an
		// unrelated sibling starting AND finishing while the watched
		// node still ran — silently disarming every pre-seeded monitor
		// for the rest of the run.
		c := newBareCoordinator(t, Spec{Watches: []string{"campaign"}}, &stubEval{}, nil)
		c.ingest(ev(1, store.EventNodeStarted, "campaign", nil))
		c.ingest(ev(2, store.EventNodeStarted, "sibling", nil))
		c.ingest(ev(3, store.EventNodeFinished, "sibling", nil))
		if !c.armed() {
			t.Fatal("supervisor disarmed by an unrelated node's start+finish while the watched node is still active")
		}
		if c.lastWatchedActive != "campaign" {
			t.Errorf("lastWatchedActive = %q; want campaign", c.lastWatchedActive)
		}
	})

	t.Run("watched node start re-arms a done supervisor", func(t *testing.T) {
		c := newBareCoordinator(t, Spec{Watches: []string{"a"}}, &stubEval{}, nil)
		c.finished = true
		c.ingest(ev(1, store.EventNodeStarted, "other", nil))
		if !c.finished {
			t.Error("unwatched node start must not re-arm")
		}
		c.ingest(ev(2, store.EventNodeStarted, "a", nil))
		if c.finished {
			t.Error("watched node start must re-arm (finished=false)")
		}
	})

	t.Run("recent ring capped at recentEventsCap", func(t *testing.T) {
		c := newBareCoordinator(t, Spec{}, &stubEval{}, nil)
		for i := 1; i <= recentEventsCap+5; i++ {
			c.ingest(ev(int64(i), store.EventToolCalled, "", nil))
		}
		if len(c.recent) != recentEventsCap {
			t.Fatalf("recent len = %d; want %d", len(c.recent), recentEventsCap)
		}
		if !strings.HasPrefix(c.recent[0], "#6 ") {
			t.Errorf("recent[0] = %q; want the ring to start at seq 6", c.recent[0])
		}
	})
}

func TestArmedWholeRun(t *testing.T) {
	c := newBareCoordinator(t, Spec{}, &stubEval{}, nil)
	if !c.armed() {
		t.Error("whole-run supervisor (empty Watches) must always be armed")
	}
}

// --- flow tests through Start/Close ---

// A whole-run supervisor (no Watches) injects run-scoped messages
// (node "") even while a node is active.
func TestCoordinatorWholeRunScopedInjection(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 16)}
	inj := &recordInjector{}
	spec := Spec{
		Name:     "global",
		Monitors: []Monitor{{EventType: "tool_error"}},
		Cooldown: time.Millisecond,
		MaxEvals: 10,
	}
	c := New(obs, inj, "r1", spec, &stubEval{}, nil)
	c.Start(context.Background())
	defer c.Close()

	obs.ch <- ev(1, store.EventNodeStarted, "some-node", nil)
	obs.ch <- ev(2, store.EventToolError, "some-node", map[string]any{"tool": "Bash"})

	waitFor(t, func() bool { return len(inj.snapshot()) == 1 })
	if got := inj.snapshot(); got[0].node != "" {
		t.Fatalf("node = %q; want run-scoped (empty)", got[0].node)
	}
}

// Events timestamped before the coordinator attached reconstruct state
// but never trigger an evaluation (catch-up is observational).
func TestCoordinatorIgnoresPreAttachHistory(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 16)}
	inj := &recordInjector{}
	eval := &stubEval{}
	spec := Spec{
		Monitors: []Monitor{{EventType: "tool_error"}},
		Cooldown: time.Millisecond,
		MaxEvals: 10,
	}
	c := New(obs, inj, "r1", spec, eval, nil)
	c.Start(context.Background())
	defer c.Close()

	old := ev(1, store.EventToolError, "n", map[string]any{"tool": "Bash"})
	old.Timestamp = time.Now().Add(-time.Hour)
	obs.ch <- old
	time.Sleep(150 * time.Millisecond)
	if n := len(inj.snapshot()); n != 0 {
		t.Fatalf("pre-attach event triggered %d injections; want 0", n)
	}

	// A fresh matching event still fires.
	obs.ch <- ev(2, store.EventToolError, "n", map[string]any{"tool": "Bash"})
	waitFor(t, func() bool { return len(inj.snapshot()) == 1 })
}

// A terminal run event closes the coordinator's worker.
func TestCoordinatorClosesOnTerminalEvent(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 4)}
	c := New(obs, &recordInjector{}, "r1", Spec{}, &stubEval{}, nil)
	c.Start(context.Background())

	obs.ch <- ev(1, store.EventRunFinished, "", nil)
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done() not closed after terminal event")
	}
	c.Close() // still safe after self-termination
}

// A closed event channel (observer went away) also ends the worker.
func TestCoordinatorClosesOnChannelClose(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event)}
	c := New(obs, &recordInjector{}, "r1", Spec{}, &stubEval{}, nil)
	c.Start(context.Background())
	close(obs.ch)
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done() not closed after event channel close")
	}
}

// After the bot declares Done, turn activity and SEEDED monitor matches
// are ignored — done means done — but a monitor the bot itself
// registered (in the Done decision or earlier) re-arms it and evaluates
// immediately. "Stop … until a registered monitor fires again" is the
// Decision schema's contract, and registered means bot-chosen.
func TestCoordinatorDoneThenMonitorRearm(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 16)}
	inj := &recordInjector{}
	eval := &scriptedEval{decisions: []*Decision{
		{Done: true, Watch: []Monitor{{EventType: "budget_warning"}}},
		{Intervene: true, Message: "back to work"},
	}}
	spec := Spec{
		Monitors: []Monitor{{EventType: "tool_error"}},
		Cooldown: time.Millisecond,
		MaxEvals: 10,
	}
	c := New(obs, inj, "r1", spec, eval, nil)
	c.Start(context.Background())
	defer c.Close()

	obs.ch <- ev(1, store.EventToolError, "n", nil)
	waitFor(t, func() bool { return eval.calls() == 1 })

	// Done: no injection happened yet.
	if n := len(inj.snapshot()); n != 0 {
		t.Fatalf("Done decision injected %d messages; want 0", n)
	}

	// The SEEDED monitor firing again does not resurrect a done
	// supervisor.
	obs.ch <- ev(2, store.EventToolError, "n", nil)
	time.Sleep(100 * time.Millisecond)
	if got := eval.calls(); got != 1 {
		t.Fatalf("seeded match resurrected a done supervisor: evals=%d, want 1", got)
	}

	// The monitor the bot REGISTERED with its Done decision re-arms it.
	obs.ch <- ev(3, store.EventBudgetWarning, "n", map[string]any{"used": 5.0})
	waitFor(t, func() bool { return len(inj.snapshot()) == 1 })
	if got := inj.snapshot(); !strings.Contains(got[0].text, "back to work") {
		t.Errorf("text = %q; want the second scripted message", got[0].text)
	}
}
