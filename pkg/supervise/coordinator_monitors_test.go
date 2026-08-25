package supervise

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The adversarial round on the pre-seeded-monitors feature confirmed
// three ways a text_contains monitor could burn the whole eval budget on
// noise: the supervisor's own injected message echoing back through the
// inbox event family, seeded matches bypassing the cooldown, and the
// bot re-registering its monitor set on every eval. These tests pin the
// fixes; each FAILED against the pre-fix coordinator.

// One intervention must not re-trigger the supervisor through the
// user_message_* events that carry its own steering text.
func TestInboxEchoDoesNotWakeMonitors(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 16)}
	inj := &recordInjector{}
	eval := &scriptedEval{decisions: []*Decision{{Intervene: true, Message: "do not call this impossible — instrument it"}}}
	spec := Spec{
		Name:     "persy",
		Watches:  []string{"campaign"},
		Monitors: []Monitor{{TextContains: "impossible"}},
		Cooldown: time.Millisecond,
		MaxEvals: 10,
	}
	c := New(obs, inj, "r1", spec, eval, nil)
	c.Start(context.Background())
	defer c.Close()

	obs.ch <- ev(1, store.EventNodeStarted, "campaign", nil)
	// Genuine trigger: the agent's own words.
	obs.ch <- ev(2, store.EventAssistantText, "campaign", map[string]any{"text": "this looks impossible"})
	waitFor(t, func() bool { return len(inj.snapshot()) == 1 })

	// The injected message (containing "impossible") echoes back as the
	// inbox family. None of these may wake another eval.
	msg := map[string]any{"id": "m1", "text": "[supervisor persy] do not call this impossible — instrument it"}
	obs.ch <- ev(3, store.EventUserMessageQueued, "campaign", msg)
	obs.ch <- ev(4, store.EventUserMessageDelivered, "campaign", msg)
	obs.ch <- ev(5, store.EventUserMessageConsumed, "campaign", msg)
	time.Sleep(150 * time.Millisecond)
	if got := eval.calls(); got != 1 {
		t.Fatalf("inbox echo woke the supervisor: evals=%d, want 1", got)
	}
	if got := len(inj.snapshot()); got != 1 {
		t.Fatalf("inbox echo caused re-injection: injections=%d, want 1", got)
	}
}

// A pre-seeded (declarative) monitor honours the cooldown: a chatty
// marker cannot drain the eval budget. A bot-REGISTERED monitor still
// bypasses it.
func TestSeededMonitorHonoursCooldownRegisteredBypasses(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 16)}
	inj := &recordInjector{}
	// Eval 1 registers a precise monitor of the bot's own choosing.
	eval := &scriptedEval{decisions: []*Decision{
		{Intervene: false, Watch: []Monitor{{TextContains: "sentinel-xyz"}}},
		{Intervene: false},
	}}
	spec := Spec{
		Watches:  []string{"campaign"},
		Monitors: []Monitor{{TextContains: "impossible"}},
		Cooldown: time.Hour, // nothing debounced gets through twice
		MaxEvals: 10,
	}
	c := New(obs, inj, "r1", spec, eval, nil)
	c.Start(context.Background())
	defer c.Close()

	obs.ch <- ev(1, store.EventNodeStarted, "campaign", nil)
	obs.ch <- ev(2, store.EventAssistantText, "campaign", map[string]any{"text": "impossible A"})
	waitFor(t, func() bool { return eval.calls() == 1 })

	// Seeded marker again, within the cooldown: suppressed.
	obs.ch <- ev(3, store.EventAssistantText, "campaign", map[string]any{"text": "impossible B"})
	time.Sleep(150 * time.Millisecond)
	if got := eval.calls(); got != 1 {
		t.Fatalf("seeded monitor bypassed the cooldown: evals=%d, want 1", got)
	}

	// Bot-registered marker: bypasses the cooldown.
	obs.ch <- ev(4, store.EventAssistantText, "campaign", map[string]any{"text": "hit sentinel-xyz"})
	waitFor(t, func() bool { return eval.calls() == 2 })
}

// A seeded match must not resurrect a supervisor whose bot declared
// itself done; only a bot-registered monitor re-arms it.
func TestSeededMatchDoesNotResurrectDone(t *testing.T) {
	eval := &scriptedEval{decisions: []*Decision{{Intervene: false}}}
	c := newBareCoordinator(t, Spec{
		Watches:  []string{"campaign"},
		Monitors: []Monitor{{TextContains: "impossible"}},
		Cooldown: time.Nanosecond,
		MaxEvals: 10,
	}, eval, nil)
	c.ingest(ev(1, store.EventNodeStarted, "campaign", nil))
	c.finished = true

	// Simulate the run-loop dispatch for a seeded match.
	if matched, registered := c.matchesMonitor(ev(2, store.EventAssistantText, "campaign", map[string]any{"text": "impossible"})); !matched || registered {
		t.Fatalf("expected a seeded-only match, got matched=%v registered=%v", matched, registered)
	}
	// The run loop skips seeded matches while finished; evaluate itself
	// also refuses.
	c.evaluate("seeded", false)
	if eval.calls() != 0 {
		t.Fatalf("done supervisor evaluated on a seeded match: evals=%d", eval.calls())
	}
}

// A cost_gt value that cannot constrain (NaN, negative — validation
// refuses them, but defense in depth) must read as "never", not fall
// through into a match-everything wildcard.
func TestNonPositiveCostGtNeverMatches(t *testing.T) {
	for _, m := range []Monitor{{CostGt: math.NaN()}, {CostGt: -1}} {
		if m.matches(ev(1, store.EventNodeStarted, "campaign", nil)) {
			t.Errorf("Monitor{CostGt:%v} matched an unrelated event — wildcard regression", m.CostGt)
		}
		if m.matches(ev(2, store.EventBudgetWarning, "campaign", map[string]any{"used": 999.0})) {
			t.Errorf("Monitor{CostGt:%v} matched a budget_warning it cannot constrain", m.CostGt)
		}
	}
}

// Re-registering an already-known monitor must not grow the set.
func TestApplyDecisionDedupsMonitors(t *testing.T) {
	c := newBareCoordinator(t, Spec{Monitors: []Monitor{{TextContains: "impossible"}}}, &stubEval{}, nil)
	m := Monitor{EventType: "tool_error", ToolName: "Bash"}
	for i := 0; i < 5; i++ {
		c.applyDecision(&Decision{Watch: []Monitor{m, {TextContains: "impossible"}}})
	}
	if got := len(c.monitors); got != 2 {
		t.Fatalf("monitors grew to %d; want 2 (1 seed + 1 registered, deduped)", got)
	}
}
