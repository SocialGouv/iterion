// E2E coverage for the issue-triage trigger flow: the REAL
// bots/issue-triage manifest is projected into a direct-mode
// consume-labels subscription, a live BoardSource tails a native store,
// and a card carrying triage:auto in inbox fires exactly one direct
// launch (with the card id in vars) while the trigger label is
// consumed. The approval gesture (needs:approval → triage:auto label
// swap, what the studio's "Approve & triage" PATCHes) re-arms the same
// path. No LLM, no dispatcher — the spine's data flow only.

package e2e

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

type recordingLauncher struct {
	mu    sync.Mutex
	plans []trigger.LaunchPlan
}

func (r *recordingLauncher) Launch(_ context.Context, p trigger.LaunchPlan) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plans = append(r.plans, p)
	return "run-triage", nil
}

func (r *recordingLauncher) snapshot() []trigger.LaunchPlan {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]trigger.LaunchPlan(nil), r.plans...)
}

func TestIssueTriageTrigger_E2E_ConsumeAndLaunch(t *testing.T) {
	// The REAL manifest is the contract under test: kind=board mode=direct
	// on inbox + triage:auto with consume_labels.
	manifest, err := bundle.LoadManifest(filepath.Join("..", "bots", "issue-triage", "manifest.yaml"))
	if err != nil {
		t.Fatalf("load issue-triage manifest: %v", err)
	}
	var sub trigger.Subscription
	found := false
	for _, inv := range bundle.EffectiveInvocations(manifest) {
		if s, ok := trigger.FromBoardInvocation("sub-triage", "", "", manifest.Name, "e2e", inv, time.Now()); ok {
			sub, found = s, true
			break
		}
	}
	if !found {
		t.Fatal("issue-triage manifest yields no board subscription")
	}
	sub.Enabled = true
	if sub.EffectiveMode() != bundle.ExecutionDirect || !sub.ConsumeLabels {
		t.Fatalf("subscription shape: mode=%q consume=%v, want direct+consume", sub.Mode, sub.ConsumeLabels)
	}

	ns, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native store: %v", err)
	}
	defer func() { _ = ns.Close() }()

	subs := trigger.NewMemorySubscriptionStore()
	if err := subs.Create(context.Background(), sub); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	launcher := &recordingLauncher{}
	eval := trigger.NewEvaluator(subs,
		trigger.WithLauncher(launcher),
		trigger.WithBoardEffect(trigger.NewNativeBoardEffect(ns, nil, nil)),
	)
	pub := publisherFunc2(func(ctx context.Context, ev trigger.Event) error { return eval.Handle(ctx, ev) })
	src := trigger.StartBoardSource(ns, pub, nil, trigger.WithBoardName("default"))
	if src == nil {
		t.Skip("board source unavailable (fsnotify not supported on host)")
	}
	defer src.Stop()

	// A trusted-author sync lands the card in inbox with triage:auto.
	iss, err := ns.Create(native.Issue{
		Title:  "Bug: CSV export panics",
		State:  native.StateInbox,
		Labels: []string{"bug", native.LabelTriageAuto},
	})
	if err != nil {
		t.Fatalf("create card: %v", err)
	}

	waitPlans := func(want int, msg string) []trigger.LaunchPlan {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if plans := launcher.snapshot(); len(plans) >= want {
				return plans
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("%s: launches=%d want %d", msg, len(launcher.snapshot()), want)
		return nil
	}

	plans := waitPlans(1, "card.created with triage:auto should direct-launch")
	if plans[0].BotID != manifest.Name {
		t.Fatalf("launched bot = %q, want %q", plans[0].BotID, manifest.Name)
	}
	if plans[0].Vars["issue_id"] != iss.ID {
		t.Fatalf("vars[issue_id] = %q, want %q", plans[0].Vars["issue_id"], iss.ID)
	}
	// The trigger label is consumed; the card's Bot stays unset (the triage
	// bot stamps it from INSIDE its run — not the spine's job here).
	card, _ := ns.Get(iss.ID)
	for _, l := range card.Labels {
		if l == native.LabelTriageAuto {
			t.Fatalf("trigger label not consumed: %v", card.Labels)
		}
	}
	if card.Bot != "" {
		t.Fatalf("spine must not stamp Bot in direct mode: %q", card.Bot)
	}

	// An unrelated content update (forge re-sync storm) must NOT re-launch:
	// the one-shot label is gone.
	title := "Bug: CSV export panics (edited)"
	if _, err := ns.Update(iss.ID, native.Patch{Title: &title}); err != nil {
		t.Fatalf("update title: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	if got := len(launcher.snapshot()); got != 1 {
		t.Fatalf("card.updated without the label re-launched: %d launches", got)
	}

	// Approval gesture: a parked card's needs:approval → triage:auto swap
	// (the studio button's PATCH) re-arms the trigger.
	labels := []string{"bug", native.LabelTriageAuto}
	if _, err := ns.Update(iss.ID, native.Patch{Labels: &labels}); err != nil {
		t.Fatalf("approve swap: %v", err)
	}
	plans = waitPlans(2, "label re-add (approval) should re-launch")
	if plans[1].Vars["issue_id"] != iss.ID {
		t.Fatalf("re-armed launch vars: %+v", plans[1].Vars)
	}
	card, _ = ns.Get(iss.ID)
	for _, l := range card.Labels {
		if l == native.LabelTriageAuto {
			t.Fatalf("re-armed trigger label not consumed: %v", card.Labels)
		}
	}
}

type publisherFunc2 func(ctx context.Context, ev trigger.Event) error

func (f publisherFunc2) Publish(ctx context.Context, ev trigger.Event) error { return f(ctx, ev) }
