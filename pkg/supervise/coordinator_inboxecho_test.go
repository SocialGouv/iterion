package supervise

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The composed steer test's quiescence phase proves the layered property
// "one settled intervention, no further model turn", whose enforcement
// no single guard owns: the run loop never matches an inbox event, and
// ingest() never lets one advance the progress fingerprint the replay
// layer reads. Either guard alone holds the property, so no eval-count
// assertion through the composed path witnesses either — muting one
// leaves every other test green, each guard being the other's backup.
// These two scenarios separate them: each is shaped so only the guard
// it names can hold it, and each reddens when that guard is muted and
// stays green when only the partner is.

// An inbox echo stays unmatched even after fresh progress invalidated
// the replay fingerprint — the run loop must never match an inbox
// event, whatever the fingerprint layer would later do with it.
func TestInboxEchoStaysUnmatchedAfterProgressAdvances(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 16)}
	inj := &recordInjector{}
	eval := &scriptedEval{decisions: []*Decision{
		{Intervene: true, Message: "impossible — instrument it"},
		{Intervene: false},
	}}
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
	obs.ch <- ev(2, store.EventAssistantText, "campaign", map[string]any{"text": "this looks impossible"})
	waitFor(t, func() bool { return len(inj.snapshot()) == 1 })

	// Progress matching no monitor and no turn boundary: it moves the
	// cursor's fingerprint and sequence, so replay suppression can no
	// longer explain a silent echo. Only the run-loop skip can.
	obs.ch <- ev(3, store.EventUsageProgress, "campaign", map[string]any{"tokens": 40000, "used": 6.2, "model": "claude-opus-5"})

	msg := map[string]any{"id": "m1", "text": "[supervisor persy] impossible — instrument it"}
	obs.ch <- ev(4, store.EventUserMessageQueued, "campaign", msg)
	obs.ch <- ev(5, store.EventUserMessageDelivered, "campaign", msg)
	obs.ch <- ev(6, store.EventUserMessageConsumed, "campaign", msg)
	time.Sleep(150 * time.Millisecond)
	if got := eval.calls(); got != 1 {
		t.Fatalf("inbox echo woke the supervisor after progress advanced: evals=%d, want 1", got)
	}
	if got := len(inj.snapshot()); got != 1 {
		t.Fatalf("inbox echo caused re-injection: injections=%d, want 1", got)
	}
}

// A redelivered trigger stays replay-suppressed even after inbox traffic
// has flowed through ingest: inbox events never advance the progress
// fingerprint, so the redelivery reads as evidence the first evaluation
// already spent — with the cooldown long expired, the fingerprint is
// the only guard left, and only the ingest-side skip keeps it true.
func TestRedeliveredTriggerStaysSuppressedPastInboxTraffic(t *testing.T) {
	obs := &fakeObserver{ch: make(chan *store.Event, 16)}
	inj := &recordInjector{}
	eval := &scriptedEval{decisions: []*Decision{
		{Intervene: true, Message: "impossible — instrument it"},
		{Intervene: false},
	}}
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
	obs.ch <- ev(2, store.EventAssistantText, "campaign", map[string]any{"text": "this looks impossible"})
	waitFor(t, func() bool { return len(inj.snapshot()) == 1 })

	msg := map[string]any{"id": "m1", "text": "[supervisor persy] impossible — instrument it"}
	obs.ch <- ev(3, store.EventUserMessageQueued, "campaign", msg)
	obs.ch <- ev(4, store.EventUserMessageDelivered, "campaign", msg)
	obs.ch <- ev(5, store.EventUserMessageConsumed, "campaign", msg)

	// Same type, node and payload as the original trigger: the transport
	// differs (a fresh seq), the evidence does not. The millisecond
	// cooldown has expired, so replay suppression alone can hold this.
	obs.ch <- ev(6, store.EventAssistantText, "campaign", map[string]any{"text": "this looks impossible"})
	time.Sleep(150 * time.Millisecond)
	if got := eval.calls(); got != 1 {
		t.Fatalf("redelivered trigger re-fired after inbox traffic: evals=%d, want 1", got)
	}
	if got := len(inj.snapshot()); got != 1 {
		t.Fatalf("redelivered trigger re-injected: injections=%d, want 1", got)
	}
}
