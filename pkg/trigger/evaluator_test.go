package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

type fakeLauncher struct{ plans []LaunchPlan }

func (f *fakeLauncher) Launch(_ context.Context, p LaunchPlan) (string, error) {
	f.plans = append(f.plans, p)
	return "run-" + p.BotID, nil
}

type fakeBoard struct{ plans []LaunchPlan }

func (f *fakeBoard) Promote(_ context.Context, p LaunchPlan) (string, error) {
	f.plans = append(f.plans, p)
	return p.Event.Subject.ID, nil
}

func seed(t *testing.T, subs ...Subscription) *MemorySubscriptionStore {
	t.Helper()
	st := NewMemorySubscriptionStore()
	for i, s := range subs {
		if s.ID == "" {
			s.ID = "sub" + time.Duration(i).String()
		}
		if err := st.Create(context.Background(), s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return st
}

func TestEvaluatorRoutesByMode(t *testing.T) {
	st := seed(t,
		Subscription{ID: "direct1", BotID: "review-pr", Mode: "direct", Enabled: true,
			Match: Matcher{Kinds: []string{KindCardMoved}}},
		Subscription{ID: "board1", BotID: "feature-dev", Mode: "board", Enabled: true,
			Match:   Matcher{SubjectStates: []string{"ready"}, Labels: []string{"feature"}},
			ArgsVar: "feature_prompt"},
		Subscription{ID: "disabled1", BotID: "noop", Mode: "board", Enabled: false,
			Match: Matcher{}},
	)
	fl, fb := &fakeLauncher{}, &fakeBoard{}
	ev := NewEvaluator(st, WithLauncher(fl), WithBoardEffect(fb))

	event := Event{
		Source:  SourceBoard,
		Kind:    KindCardMoved,
		Labels:  []string{"feature"},
		Subject: Subject{Type: "card", ID: "card7", Title: "Add export", Body: "as CSV", State: "ready"},
	}
	if err := ev.Handle(context.Background(), event); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(fl.plans) != 1 || fl.plans[0].BotID != "review-pr" {
		t.Fatalf("direct launcher plans = %+v, want one review-pr", fl.plans)
	}
	if len(fb.plans) != 1 || fb.plans[0].BotID != "feature-dev" {
		t.Fatalf("board plans = %+v, want one feature-dev", fb.plans)
	}
	// disabled subscription must not fire even though Matcher{} matches.
	if got := fb.plans[0].Vars["feature_prompt"]; got != "Add export\n\nas CSV" {
		t.Fatalf("ArgsVar payload = %q, want title+body", got)
	}
}

func TestEvaluatorRunCompletionChaining(t *testing.T) {
	// "runned by iterion": when feature-dev finishes, fire review-pr.
	st := seed(t, Subscription{
		ID: "chain", BotID: "review-pr", Mode: "direct", Enabled: true,
		Match: Matcher{
			Sources: []Source{SourceRun},
			Kinds:   []string{KindRunFinished},
			Authors: []string{"feature-dev"}, // Actor carries the upstream bot id
		},
	})
	fl, fb := &fakeLauncher{}, &fakeBoard{}
	ev := NewEvaluator(st, WithLauncher(fl), WithBoardEffect(fb))

	// A finished feature-dev run fires the chain.
	_ = ev.Handle(context.Background(), Event{
		Source: SourceRun, Kind: KindRunFinished, Actor: "feature-dev",
		Subject: Subject{Type: "run", ID: "run-123", State: "finished"},
	})
	if len(fl.plans) != 1 || fl.plans[0].BotID != "review-pr" {
		t.Fatalf("run-completion chain plans = %+v, want one review-pr", fl.plans)
	}
	if len(fb.plans) != 0 {
		t.Fatalf("board effect should not fire on a run event: %+v", fb.plans)
	}

	// A FAILED run of the same bot must not fire this (finished-only) chain.
	fl.plans = nil
	_ = ev.Handle(context.Background(), Event{
		Source: SourceRun, Kind: KindRunFailed, Actor: "feature-dev",
		Subject: Subject{Type: "run", ID: "run-124", State: "failed"},
	})
	if len(fl.plans) != 0 {
		t.Fatalf("failed run should not fire a finished-only chain: %+v", fl.plans)
	}
}

func TestEvaluatorMergesEventVarsUnderSubscriptionVars(t *testing.T) {
	// A forge/custom source stamps dynamic vars under payload["vars"]; the
	// subscription's static Vars override on key collision (operator pins win).
	st := seed(t, Subscription{
		ID: "s", BotID: "review-pr", Mode: "direct", Enabled: true,
		Match: Matcher{Sources: []Source{SourceCustom}},
		Vars:  map[string]string{"reviewer": "strict"}, // operator pin
	})
	fl := &fakeLauncher{}
	ev := NewEvaluator(st, WithLauncher(fl))

	_ = ev.Handle(context.Background(), Event{
		Source: SourceCustom, Kind: "ci.done",
		Payload: map[string]any{"vars": map[string]any{"pr_url": "http://x/1", "reviewer": "lax"}},
		Subject: Subject{Type: "custom", ID: "c1"},
	})
	if len(fl.plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(fl.plans))
	}
	got := fl.plans[0].Vars
	if got["pr_url"] != "http://x/1" {
		t.Fatalf("dynamic event var dropped: %+v", got)
	}
	if got["reviewer"] != "strict" {
		t.Fatalf("subscription var should win on collision; got reviewer=%q", got["reviewer"])
	}
}

func TestEvaluatorSkipsAlreadyLaunchedEvent(t *testing.T) {
	// A forge event the inline webhook already launched (carries
	// launched_run_id) must never be re-launched, even if a forge
	// subscription matches it — guarantees the observational emit can't
	// double-launch.
	st := seed(t, Subscription{
		ID: "forge", BotID: "review-pr", Mode: "direct", Enabled: true,
		Match: Matcher{Sources: []Source{SourceForge}},
	})
	fl := &fakeLauncher{}
	ev := NewEvaluator(st, WithLauncher(fl))

	_ = ev.Handle(context.Background(), Event{
		Source:  SourceForge,
		Kind:    "pull_request",
		Action:  "opened",
		Payload: map[string]any{PayloadLaunchedRunID: "run-existing"},
		Subject: Subject{Type: "pull_request", ID: "pr:7"},
	})
	if len(fl.plans) != 0 {
		t.Fatalf("already-launched forge event re-launched: %+v", fl.plans)
	}

	// The same event WITHOUT the marker (the future cutover) does launch.
	_ = ev.Handle(context.Background(), Event{
		Source: SourceForge, Kind: "pull_request", Action: "opened",
		Subject: Subject{Type: "pull_request", ID: "pr:7"},
	})
	if len(fl.plans) != 1 || fl.plans[0].BotID != "review-pr" {
		t.Fatalf("unmarked forge event should launch: %+v", fl.plans)
	}
}

func TestEvaluatorSkipsWhenEffectMissing(t *testing.T) {
	st := seed(t, Subscription{ID: "b", BotID: "x", Mode: "board", Enabled: true, Match: Matcher{}})
	ev := NewEvaluator(st) // no board effect wired
	// Must not panic / error; just skip with a warn (logger nil).
	if err := ev.Handle(context.Background(), Event{Source: SourceBoard, Subject: Subject{ID: "c1"}}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestListCandidatesScoping(t *testing.T) {
	st := seed(t,
		Subscription{ID: "repo", TenantID: "t1", Repo: "acme/w", BotID: "a", Enabled: true},
		Subscription{ID: "wide", TenantID: "t1", Repo: "", BotID: "b", Enabled: true},
		Subscription{ID: "other", TenantID: "t1", Repo: "other/x", BotID: "c", Enabled: true},
		Subscription{ID: "offt", TenantID: "t2", Repo: "acme/w", BotID: "d", Enabled: true},
		Subscription{ID: "disabled", TenantID: "t1", Repo: "acme/w", BotID: "e", Enabled: false},
	)
	got, err := st.ListCandidates(context.Background(), Event{TenantID: "t1", Repo: "acme/w"})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	if !ids["repo"] || !ids["wide"] {
		t.Fatalf("want repo+wide, got %v", ids)
	}
	if ids["other"] || ids["offt"] || ids["disabled"] {
		t.Fatalf("unexpected candidate in %v", ids)
	}
}

// No bus event fires a connector subscription — even one whose matcher is
// empty and so matches everything — and the bus never evaluates a connector
// event or one marked observational (ADR-168 §8: the connector worker
// matches from the intake; readers ship before any publisher sets the mark).
func TestTheBusNeverFiresAConnectorSubscription(t *testing.T) {
	st := seed(t,
		Subscription{ID: "conn", TenantID: "", BotID: "chat-bot", Enabled: true, Invocation: bundle.InvocationKindConnector,
			ConnectionID: "conn-a", TriggerID: "slack.message.posted"},
		Subscription{ID: "wide", BotID: "wide-bot", Mode: "direct", Enabled: true},
	)
	fl := &fakeLauncher{}
	ev := NewEvaluator(st, WithLauncher(fl))
	for _, e := range []Event{
		{Source: SourceBoard, Kind: KindCardMoved, Subject: Subject{Type: "card", ID: "c1"}},
		{Source: SourceCustom, Kind: "anything"},
	} {
		_ = ev.Handle(context.Background(), e)
	}
	for _, p := range fl.plans {
		if p.BotID == "chat-bot" {
			t.Fatalf("a bus event fired the connector subscription: %+v", p)
		}
	}
	if len(fl.plans) != 2 {
		t.Fatalf("the control subscription fired %d times, want 2", len(fl.plans))
	}
	fl.plans = nil
	for _, e := range []Event{
		{Source: SourceConnector, Kind: "slack.message.posted", ConnectionID: "conn-a"},
		{Source: SourceCustom, Kind: "anything", Payload: map[string]any{PayloadObservational: true}},
	} {
		_ = ev.Handle(context.Background(), e)
	}
	if len(fl.plans) != 0 {
		t.Fatalf("a connector or observational event was evaluated: %+v", fl.plans)
	}
}
