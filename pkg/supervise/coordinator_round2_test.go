package supervise

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Round-2 regression pins: a cooldown-suppressed seeded match must be
// DEFERRED (not dropped), failed evaluations must not consume the eval
// budget, and a terminal run must refuse late steering. Each test was
// seen RED against the pre-fix code.

// A seeded marker that fires inside the cooldown window is evaluated at
// the cooldown's expiry, carrying the trigger in its wake reason — the
// signal is delayed, never lost.
func TestSeededMatchInsideCooldownIsDeferredNotDropped(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 16)}
	inj := &recordInjector{}
	eval := &scriptedEval{decisions: []*Decision{{Intervene: false}}}
	spec := Spec{
		Watches:  []string{"campaign"},
		Monitors: []Monitor{{EventType: "budget_warning"}, {TextContains: "impossible"}},
		Cooldown: 700 * time.Millisecond,
		MaxEvals: 10,
	}
	c := New(obs, inj, "r1", spec, eval, nil)
	c.Start(context.Background())
	defer c.Close()

	obs.ch <- ev(1, store.EventNodeStarted, "campaign", nil)
	// Eval 1 opens the cooldown window.
	obs.ch <- ev(2, store.EventBudgetWarning, "campaign", map[string]any{"used": 1.0})
	waitFor(t, func() bool { return eval.calls() == 1 })

	// The give-up marker lands INSIDE the window — one-shot, never
	// repeated (the agent says it once).
	obs.ch <- ev(3, store.EventAssistantText, "campaign", map[string]any{"text": "this is impossible, I'll stop here"})

	// It must still be evaluated once the cooldown expires.
	waitFor(t, func() bool { return eval.calls() == 2 })
	eval.mu.Lock()
	wake := eval.inputs[1].WakeReason
	eval.mu.Unlock()
	if !strings.Contains(wake, "impossible") || !strings.Contains(wake, "deferred by cooldown") {
		t.Fatalf("deferred wake lost its trigger: %q", wake)
	}
}

// The deferred wake survives the watched node finishing before the
// cooldown expires: the marker is typically emitted as a pass wraps up
// (seconds before node_finished), and the next pass — a loop back-edge
// re-entering the same node — must still hear about it.
func TestDeferredWakeSurvivesNodeFinish(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 16)}
	inj := &recordInjector{}
	eval := &scriptedEval{decisions: []*Decision{{Intervene: false}}}
	spec := Spec{
		Watches:  []string{"campaign"},
		Monitors: []Monitor{{EventType: "budget_warning"}, {TextContains: "impossible"}},
		Cooldown: 500 * time.Millisecond,
		MaxEvals: 10,
	}
	c := New(obs, inj, "r1", spec, eval, nil)
	c.Start(context.Background())
	defer c.Close()

	obs.ch <- ev(1, store.EventNodeStarted, "campaign", nil)
	obs.ch <- ev(2, store.EventBudgetWarning, "campaign", map[string]any{"used": 1.0})
	waitFor(t, func() bool { return eval.calls() == 1 })

	// Marker inside the cooldown, then the node finishes (end of pass).
	obs.ch <- ev(3, store.EventAssistantText, "campaign", map[string]any{"text": "this is impossible, I stop"})
	obs.ch <- ev(4, store.EventNodeFinished, "campaign", nil)

	// Wait past the cooldown while disarmed: the pending wake must NOT
	// fire (and must not be lost either).
	time.Sleep(900 * time.Millisecond)
	if got := eval.calls(); got != 1 {
		t.Fatalf("disarmed pending wake fired: evals=%d", got)
	}

	// Next pass re-enters the watched node: the deferred marker is
	// re-armed and evaluated.
	obs.ch <- ev(5, store.EventNodeStarted, "campaign", nil)
	waitFor(t, func() bool { return eval.calls() == 2 })
	eval.mu.Lock()
	wake := eval.inputs[1].WakeReason
	eval.mu.Unlock()
	if !strings.Contains(wake, "impossible") {
		t.Fatalf("re-armed wake lost its trigger: %q", wake)
	}
}

// A failed evaluation (transport/auth) must not consume the MaxEvals
// budget; three consecutive failures park supervision loudly instead of
// burning it on nothing.
func TestFailedEvaluationsDoNotConsumeBudget(t *testing.T) {
	failing := &errEval{err: errors.New("api error 401")}
	c := newBareCoordinator(t, Spec{Watches: []string{"campaign"}, Cooldown: time.Nanosecond, MaxEvals: 10}, failing, nil)
	c.ingest(ev(1, store.EventNodeStarted, "campaign", nil))
	for i := 0; i < 6; i++ {
		c.evaluate("monitor matched: x", true)
	}
	if c.evalCount != 0 {
		t.Errorf("failed evals consumed the budget: evalCount=%d, want 0", c.evalCount)
	}
	if failing.calls != maxEvalFailures {
		t.Errorf("evaluator called %d times; want %d (then parked)", failing.calls, maxEvalFailures)
	}
}

type errEval struct {
	err   error
	calls int
}

func (e *errEval) Evaluate(_ context.Context, _ EvalInput) (*Decision, EvalUsage, error) {
	e.calls++
	return nil, EvalUsage{}, e.err
}

// StoreInjector must refuse to queue steering into a terminal run — an
// eval finishing after run end would otherwise park a stale message the
// next resume drains into a fresh pass.
func TestStoreInjectorRefusesTerminalRun(t *testing.T) {
	fs := &fakeRunStore{run: &store.Run{ID: "r1", Status: store.RunStatusFailed}}
	inj := &StoreInjector{Store: fs}
	if err := inj.Inject(context.Background(), "r1", "campaign", "keep going"); err == nil {
		t.Fatal("Inject into a failed run succeeded; want refusal")
	}
	if fs.appended {
		t.Fatal("a message was appended to a terminal run")
	}
	// A live run still accepts.
	fs.run.Status = store.RunStatusRunning
	if err := inj.Inject(context.Background(), "r1", "campaign", "keep going"); err != nil {
		t.Fatalf("Inject into a running run failed: %v", err)
	}
	if !fs.appended {
		t.Fatal("no message appended to the running run")
	}
}

// fakeRunStore implements just the two methods StoreInjector touches;
// anything else panics via the embedded nil interface.
type fakeRunStore struct {
	store.RunStore
	run      *store.Run
	appended bool
}

func (f *fakeRunStore) LoadRun(_ context.Context, _ string) (*store.Run, error) {
	return f.run, nil
}

func (f *fakeRunStore) AppendQueuedMessage(_ context.Context, _ string, _ store.QueuedUserMessage) error {
	f.appended = true
	return nil
}

func (f *fakeRunStore) AppendEvent(_ context.Context, _ string, evt store.Event) (*store.Event, error) {
	return &evt, nil
}
