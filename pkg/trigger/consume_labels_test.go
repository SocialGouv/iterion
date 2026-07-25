package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// triageSub is the canonical consume-labels trigger: when a card in inbox
// carries triage:auto, launch issue-triage ON it (direct mode) after
// consuming the label.
func triageSub() Subscription {
	return Subscription{
		ID:            "triage",
		BotID:         "issue-triage",
		Invocation:    "board",
		Mode:          "direct",
		ConsumeLabels: true,
		Enabled:       true,
		Match: Matcher{
			Sources:       []Source{SourceBoard},
			Kinds:         []string{KindCardCreated, KindCardUpdated},
			SubjectStates: []string{native.StateInbox},
			Labels:        []string{"triage:auto"},
		},
	}
}

func TestFromBoardInvocationModeDirect(t *testing.T) {
	now := time.Now()
	inv := bundle.Invocation{
		Kind: bundle.InvocationKindBoard,
		Mode: bundle.ExecutionDirect,
		Board: &bundle.InvocationBoard{
			On:            []string{bundle.BoardKindCardCreated},
			ToStates:      []string{"inbox"},
			AllLabels:     []string{"triage:auto"},
			ConsumeLabels: true,
		},
	}
	sub, ok := FromBoardInvocation("id1", "t1", "acme/w", "issue-triage", "operator", inv, now)
	if !ok {
		t.Fatalf("FromBoardInvocation returned ok=false")
	}
	if sub.Mode != bundle.ExecutionDirect {
		t.Fatalf("mode = %q, want direct", sub.Mode)
	}
	if !sub.ConsumeLabels {
		t.Fatalf("ConsumeLabels not carried over")
	}

	// Default (no mode) keeps the historical promote semantics and must NOT
	// pick up consume_labels (promote is already idempotent).
	inv.Mode = ""
	sub, ok = FromBoardInvocation("id2", "t1", "acme/w", "feature-dev", "operator", inv, now)
	if !ok || sub.Mode != bundle.ExecutionBoard {
		t.Fatalf("default mode = %q ok=%v, want board", sub.Mode, ok)
	}
	if sub.ConsumeLabels {
		t.Fatalf("board-mode subscription must not consume labels")
	}
}

// TestEvaluatorConsumeLabelsOneShot is the no-double-launch invariant for
// direct board triggers: the trigger label is stripped before the first
// launch, so replaying the same event N times launches exactly once, and the
// launched plan carries the target card in vars["issue_id"].
func TestEvaluatorConsumeLabelsOneShot(t *testing.T) {
	ns := newBoardStore(t)
	iss, err := ns.Create(native.Issue{Title: "Bug: export broken", State: native.StateInbox, Labels: []string{"bug", "triage:auto"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	subs := NewMemorySubscriptionStore()
	_ = subs.Create(context.Background(), triageSub())
	fl := &fakeLauncher{}
	eval := NewEvaluator(subs, WithLauncher(fl), WithBoardEffect(NewNativeBoardEffect(ns, nil, nil)))

	ev := Event{
		Source:  SourceBoard,
		Kind:    KindCardCreated,
		Labels:  []string{"bug", "triage:auto"},
		Subject: Subject{Type: "card", ID: iss.ID, Title: iss.Title, State: native.StateInbox},
	}
	for i := 0; i < 3; i++ {
		if err := eval.Handle(context.Background(), ev); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}

	if len(fl.plans) != 1 {
		t.Fatalf("launched %d times, want exactly 1 (one-shot)", len(fl.plans))
	}
	if got := fl.plans[0].Vars["issue_id"]; got != iss.ID {
		t.Fatalf("vars[issue_id] = %q, want %q", got, iss.ID)
	}
	card, _ := ns.Get(iss.ID)
	if containsStr(card.Labels, "triage:auto", true) {
		t.Fatalf("trigger label not consumed: %v", card.Labels)
	}
	if !containsStr(card.Labels, "bug", true) {
		t.Fatalf("unrelated label lost: %v", card.Labels)
	}
	if card.Bot != "" {
		t.Fatalf("direct mode must not stamp the card's Bot; got %q", card.Bot)
	}

	// Re-adding the label re-arms the trigger.
	relabeled := append(append([]string(nil), card.Labels...), "triage:auto")
	if _, err := ns.Update(iss.ID, native.Patch{Labels: &relabeled}); err != nil {
		t.Fatalf("relabel: %v", err)
	}
	ev.Kind = KindCardUpdated
	if err := eval.Handle(context.Background(), ev); err != nil {
		t.Fatalf("handle rearmed: %v", err)
	}
	if len(fl.plans) != 2 {
		t.Fatalf("re-armed trigger launched %d times total, want 2", len(fl.plans))
	}
}

// TestEvaluatorConsumeLabelsRequiresConsumer: a consume_labels subscription
// with a board effect that cannot consume (or none at all) must skip the
// launch — honest-fail, never a launch storm.
func TestEvaluatorConsumeLabelsRequiresConsumer(t *testing.T) {
	subs := NewMemorySubscriptionStore()
	_ = subs.Create(context.Background(), triageSub())
	fl := &fakeLauncher{}
	// fakeBoard implements BoardEffect but NOT LabelConsumer.
	eval := NewEvaluator(subs, WithLauncher(fl), WithBoardEffect(&fakeBoard{}))

	ev := Event{
		Source: SourceBoard, Kind: KindCardCreated, Labels: []string{"triage:auto"},
		Subject: Subject{Type: "card", ID: "c1", State: native.StateInbox},
	}
	if err := eval.Handle(context.Background(), ev); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(fl.plans) != 0 {
		t.Fatalf("launched without consuming: %+v", fl.plans)
	}

	// No board effect at all: same skip.
	eval = NewEvaluator(subs, WithLauncher(fl))
	if err := eval.Handle(context.Background(), ev); err != nil {
		t.Fatalf("handle no-effect: %v", err)
	}
	if len(fl.plans) != 0 {
		t.Fatalf("launched with no board effect: %+v", fl.plans)
	}
}

// TestBuildPlanIssueIDOnlyForDirect: the promote path must not leak
// vars["issue_id"] into every promoted card's BotArgs.
func TestBuildPlanIssueIDOnlyForDirect(t *testing.T) {
	ns := newBoardStore(t)
	iss, _ := ns.Create(native.Issue{Title: "Add CSV export", State: native.StateReady, Labels: []string{"feature"}})
	subs := NewMemorySubscriptionStore()
	_ = subs.Create(context.Background(), boardSub())
	eval := NewEvaluator(subs, WithBoardEffect(NewNativeBoardEffect(ns, nil, nil)))

	_ = eval.Handle(context.Background(), Event{
		Source: SourceBoard, Kind: KindCardMoved, Labels: []string{"feature"},
		Subject: Subject{Type: "card", ID: iss.ID, Title: iss.Title, State: native.StateReady},
	})
	card, _ := ns.Get(iss.ID)
	if card.Bot != "feature-dev" {
		t.Fatalf("promote did not stamp bot: %q", card.Bot)
	}
	if _, ok := card.BotArgs["issue_id"]; ok {
		t.Fatalf("promote leaked issue_id into BotArgs: %v", card.BotArgs)
	}
}
