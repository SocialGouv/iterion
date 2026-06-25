package trigger

import (
	"context"
	"testing"
	"time"
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
