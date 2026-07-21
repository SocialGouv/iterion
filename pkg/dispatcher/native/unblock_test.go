package native_test

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

func TestPromoteUnblockedDependentsToBacklog(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mesh, err := s.Create(native.Issue{Title: "mesh", State: native.StateInProgress, Bot: "mesh"})
	if err != nil {
		t.Fatal(err)
	}
	feature, err := s.Create(native.Issue{
		Title:    "feature",
		State:    native.StateWaitingDeps,
		Bot:      "feature",
		Blockers: []string{mesh.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetState(mesh.ID, native.StateDone); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(feature.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != native.StateBacklog {
		t.Fatalf("feature state = %q, want backlog after unblock", got.State)
	}

	// Audit: issue_unblocked event present.
	found := false
	_ = s.ScanEvents(func(ev *native.Event) bool {
		if ev.Type == native.EvtIssueUnblocked && ev.IssueID == feature.ID {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatal("expected issue_unblocked event")
	}
}

func TestPromoteUnblockedDependentsAutoReady(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mesh, _ := s.Create(native.Issue{Title: "mesh", State: native.StateInProgress, Bot: "mesh"})
	feature, _ := s.Create(native.Issue{
		Title:    "feature",
		State:    native.StateWaitingDeps,
		Bot:      "feature",
		Blockers: []string{mesh.ID},
		BotArgs:  map[string]string{"auto_ready": "true"},
	})
	if _, err := s.SetState(mesh.ID, native.StateDone); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(feature.ID)
	if got.State != native.StateReady {
		t.Fatalf("auto_ready feature state = %q, want ready", got.State)
	}
	if !native.CanLaunch(s, got) {
		t.Fatal("auto_ready feature must be CanLaunch after unblock")
	}
}

func TestCreateRejectsBlockerCycle(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Create(native.Issue{Title: "a", State: native.StateBacklog})
	b, err := s.Create(native.Issue{Title: "b", State: native.StateBacklog, Blockers: []string{a.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// Patch a to block on b → cycle.
	blockers := []string{b.ID}
	if _, err := s.Update(a.ID, native.Patch{Blockers: &blockers}); err == nil {
		t.Fatal("expected cycle rejection on Update")
	}
}

func TestWaitingDepsNotDispatched(t *testing.T) {
	a, s := newAdapter(t)
	_, _ = s.Create(native.Issue{
		Title: "waiting",
		State: native.StateWaitingDeps,
		Bot:   "feature",
	})
	// Also put a ready free ticket so the board isn't empty of eligibles.
	ready, _ := s.Create(native.Issue{Title: "go", State: native.StateReady, Bot: "mesh"})
	cands, err := a.ListCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.ID != ready.ID {
			t.Fatalf("unexpected candidate %s", c.ID)
		}
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
}
