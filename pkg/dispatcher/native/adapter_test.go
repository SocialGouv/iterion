package native_test

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

func newAdapter(t *testing.T) (*native.Adapter, *native.Store) {
	t.Helper()
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return native.NewAdapter(s), s
}

func TestListCandidatesEligibleOnly(t *testing.T) {
	a, s := newAdapter(t)

	ready, _ := s.Create(native.Issue{Title: "go", State: "ready"})
	_, _ = s.Create(native.Issue{Title: "later", State: "backlog"})
	claimed, _ := s.Create(native.Issue{Title: "taken", State: "ready"})
	if err := s.Claim(claimed.ID, "other"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	got, err := a.ListCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(got) != 1 || got[0].ID != ready.ID {
		t.Fatalf("want only ready, got %+v", got)
	}
	if got[0].WorkflowState != "ready" {
		t.Fatalf("workflow state not propagated: %s", got[0].WorkflowState)
	}
}

func TestListCandidatesBlockerGating(t *testing.T) {
	a, s := newAdapter(t)

	blocker, _ := s.Create(native.Issue{Title: "blocker", State: "in_progress"})
	gated, _ := s.Create(native.Issue{Title: "needs blocker", State: "ready", Blockers: []string{blocker.ID}})

	candidates, _ := a.ListCandidates(context.Background())
	for _, c := range candidates {
		if c.ID == gated.ID {
			t.Fatalf("blocked issue should not be a candidate")
		}
	}

	if _, err := s.SetState(blocker.ID, "done"); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	candidates, _ = a.ListCandidates(context.Background())
	found := false
	for _, c := range candidates {
		if c.ID == gated.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("gated issue should be a candidate after blocker closed")
	}
}

// Terminal non-success (StateBlocked) must NOT satisfy a hard dep — same rule
// as CanLaunch / the pipeline launch loop.
func TestListCandidatesBlockedDoesNotSatisfy(t *testing.T) {
	a, s := newAdapter(t)

	wont, _ := s.Create(native.Issue{Title: "won't do", State: native.StateBlocked})
	gated, _ := s.Create(native.Issue{
		Title: "needs real done", State: native.StateReady, Blockers: []string{wont.ID},
	})
	candidates, _ := a.ListCandidates(context.Background())
	for _, c := range candidates {
		if c.ID == gated.ID {
			t.Fatal("blocker in terminal blocked must not make dependent eligible")
		}
	}
}

func TestListCandidatesMissingBlockerFailClosed(t *testing.T) {
	a, s := newAdapter(t)
	gated, _ := s.Create(native.Issue{
		Title: "dangling", State: native.StateReady, Blockers: []string{"native:missing"},
	})
	candidates, _ := a.ListCandidates(context.Background())
	for _, c := range candidates {
		if c.ID == gated.ID {
			t.Fatal("missing blocker must fail closed")
		}
	}
}

func TestRefreshStates(t *testing.T) {
	a, s := newAdapter(t)
	x, _ := s.Create(native.Issue{Title: "x", State: "ready"})
	y, _ := s.Create(native.Issue{Title: "y", State: "in_progress"})

	got, err := a.RefreshStates(context.Background(), []string{x.ID, y.ID, "native:missing"})
	if err != nil {
		t.Fatalf("RefreshStates: %v", err)
	}
	if got[x.ID] != "ready" || got[y.ID] != "in_progress" {
		t.Fatalf("bad states: %v", got)
	}
	if _, ok := got["native:missing"]; ok {
		t.Fatal("missing ID should be omitted")
	}
}

func TestAdapterClaimRelease(t *testing.T) {
	a, s := newAdapter(t)
	iss, _ := s.Create(native.Issue{Title: "x", State: "ready"})

	if err := a.Claim(context.Background(), iss.ID, "host-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := a.Claim(context.Background(), iss.ID, "host-2"); !errors.Is(err, tracker.ErrClaimConflict) {
		t.Fatalf("want ErrClaimConflict, got %v", err)
	}
	if err := a.Release(context.Background(), iss.ID, "host-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := a.Claim(context.Background(), iss.ID, "host-2"); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
}

func TestAdapterComment(t *testing.T) {
	a, s := newAdapter(t)
	iss, _ := s.Create(native.Issue{Title: "x", State: "ready"})

	// Commenting on a real issue appends to its thread under the
	// "dispatcher" author.
	if err := a.Comment(context.Background(), iss.ID, "MR opened: http://x/1"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	got, _ := s.Get(iss.ID)
	if len(got.Comments) != 1 || got.Comments[0].Author != "dispatcher" {
		t.Fatalf("comment not appended by dispatcher: %+v", got.Comments)
	}

	// Commenting on a missing issue surfaces ErrNotFound.
	if err := a.Comment(context.Background(), "native:nope", "hi"); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListForRepark(t *testing.T) {
	a, s := newAdapter(t)
	ready, _ := s.Create(native.Issue{Title: "ready", State: native.StateReady})
	if err := s.SetLastRun(ready.ID, "run-ready", ""); err != nil {
		t.Fatalf("SetLastRun ready: %v", err)
	}
	progress, _ := s.Create(native.Issue{Title: "wip", State: native.StateInProgress})
	if err := s.SetLastRun(progress.ID, "run-wip", ""); err != nil {
		t.Fatalf("SetLastRun progress: %v", err)
	}
	parked, _ := s.Create(native.Issue{Title: "parked", State: native.StateAwaitingInput})
	if err := s.SetLastRun(parked.ID, "run-parked", ""); err != nil {
		t.Fatalf("SetLastRun parked: %v", err)
	}
	theirs, _ := s.Create(native.Issue{Title: "theirs", State: native.StateInProgress})
	if err := s.SetLastRun(theirs.ID, "run-theirs", ""); err != nil {
		t.Fatalf("SetLastRun theirs: %v", err)
	}
	if err := s.Claim(theirs.ID, "other-host"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	ours, _ := s.Create(native.Issue{Title: "ours", State: native.StateInProgress})
	if err := s.SetLastRun(ours.ID, "run-ours", ""); err != nil {
		t.Fatalf("SetLastRun ours: %v", err)
	}
	if err := s.Claim(ours.ID, "me"); err != nil {
		t.Fatalf("Claim ours: %v", err)
	}
	fresh, _ := s.Create(native.Issue{Title: "fresh", State: native.StateReady})
	_, _ = s.Create(native.Issue{Title: "inbox", State: native.StateInbox})

	got, err := a.ListForRepark("me")
	if err != nil {
		t.Fatalf("ListForRepark: %v", err)
	}
	ids := map[string]bool{}
	for _, iss := range got {
		ids[iss.ID] = true
	}
	if !ids[ready.ID] || !ids[progress.ID] {
		t.Fatalf("want ready + in_progress with last_run, got %+v", ids)
	}
	if ids[parked.ID] {
		t.Error("awaiting_input belongs to ListAwaitingInput, not ListForRepark")
	}
	if ids[theirs.ID] {
		t.Error("another daemon's claim must be excluded")
	}
	if ids[ours.ID] {
		t.Error("an already-handled claim from this daemon must not be re-parked every tick")
	}
	if ids[fresh.ID] {
		t.Error("a card with no last_run cannot be re-parked")
	}
}

func TestAdapterUpdateState(t *testing.T) {
	a, s := newAdapter(t)
	iss, _ := s.Create(native.Issue{Title: "x", State: "ready"})
	if err := a.UpdateState(context.Background(), iss.ID, "in_progress"); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	got, _ := s.Get(iss.ID)
	if got.State != "in_progress" {
		t.Fatalf("state not updated: %s", got.State)
	}
}

// Compile-time assertion that *Adapter satisfies tracker.Tracker.
var _ tracker.Tracker = (*native.Adapter)(nil)
