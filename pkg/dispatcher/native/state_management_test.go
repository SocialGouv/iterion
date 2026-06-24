package native

import (
	"errors"
	"testing"
)

// countState returns how many indexed issues are currently in `state`.
func countState(t *testing.T, s *Store, state string) int {
	t.Helper()
	issues, err := s.List(ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	n := 0
	for _, iss := range issues {
		if iss.State == state {
			n++
		}
	}
	return n
}

func TestAddState(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddState(State{Name: "triage", Display: "Triage"}); err != nil {
		t.Fatalf("AddState: %v", err)
	}
	if s.Board().StateByName("triage") == nil {
		t.Fatal("triage not added")
	}
	// Appended last.
	states := s.Board().States
	if states[len(states)-1].Name != "triage" {
		t.Fatalf("triage not appended last: %v", states)
	}
	// Duplicate rejected.
	if err := s.AddState(State{Name: "triage"}); err == nil {
		t.Fatal("expected duplicate rejection")
	}
	// Empty name rejected.
	if err := s.AddState(State{Name: ""}); err == nil {
		t.Fatal("expected empty-name rejection")
	}
}

func TestRenameStateCascades(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Create(Issue{Title: "a", State: "backlog"})
	b, _ := s.Create(Issue{Title: "b", State: "backlog"})
	_, _ = s.Create(Issue{Title: "c", State: "ready"})

	touched, err := s.RenameState("backlog", "todo")
	if err != nil {
		t.Fatalf("RenameState: %v", err)
	}
	if touched != 2 {
		t.Fatalf("touched = %d, want 2", touched)
	}
	if s.Board().StateByName("backlog") != nil {
		t.Fatal("old state still present")
	}
	if s.Board().StateByName("todo") == nil {
		t.Fatal("new state missing")
	}
	for _, id := range []string{a.ID, b.ID} {
		got, _ := s.Get(id)
		if got.State != "todo" {
			t.Fatalf("issue %s state = %q, want todo", id, got.State)
		}
	}

	// Per-issue rename events emitted with the right reason.
	renamed := 0
	_ = s.ScanEvents(func(e *Event) bool {
		if e.Type == EvtIssueState && e.Payload["reason"] == "state_rename" {
			renamed++
		}
		return true
	})
	if renamed != 2 {
		t.Fatalf("state_rename events = %d, want 2", renamed)
	}
}

func TestRenameStateEdgeCases(t *testing.T) {
	s := newTestStore(t)
	// rename to self → no-op
	if n, err := s.RenameState("ready", "ready"); err != nil || n != 0 {
		t.Fatalf("self-rename: n=%d err=%v", n, err)
	}
	// rename unknown → error
	if _, err := s.RenameState("nope", "x"); err == nil {
		t.Fatal("expected unknown-state error")
	}
	// rename onto existing → refused
	if _, err := s.RenameState("ready", "done"); err == nil {
		t.Fatal("expected onto-existing rejection")
	}
	// empty names → error
	if _, err := s.RenameState("", "x"); err == nil {
		t.Fatal("expected empty-name rejection")
	}
}

func TestDeleteStateEmpty(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.DeleteState("review", ""); err != nil {
		t.Fatalf("DeleteState empty: %v", err)
	}
	if s.Board().StateByName("review") != nil {
		t.Fatal("review not removed")
	}
}

func TestDeleteStateNonEmptyRequiresTarget(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.Create(Issue{Title: "a", State: "backlog"})

	if _, err := s.DeleteState("backlog", ""); !errors.Is(err, ErrStateNotEmpty) {
		t.Fatalf("want ErrStateNotEmpty, got %v", err)
	}
	// still present after the refused delete
	if s.Board().StateByName("backlog") == nil {
		t.Fatal("backlog wrongly removed on refused delete")
	}

	touched, err := s.DeleteState("backlog", "ready")
	if err != nil {
		t.Fatalf("DeleteState with target: %v", err)
	}
	if touched != 1 {
		t.Fatalf("touched = %d, want 1", touched)
	}
	if s.Board().StateByName("backlog") != nil {
		t.Fatal("backlog not removed")
	}
	if countState(t, s, "ready") != 1 {
		t.Fatal("issue not migrated to ready")
	}
}

func TestDeleteStateGuards(t *testing.T) {
	s := newTestStore(t)
	// unknown state
	if _, err := s.DeleteState("nope", ""); err == nil {
		t.Fatal("expected unknown-state error")
	}
	_, _ = s.Create(Issue{Title: "a", State: "backlog"})
	// bad migration target
	if _, err := s.DeleteState("backlog", "nowhere"); err == nil {
		t.Fatal("expected bad-target error")
	}
	// target == name
	if _, err := s.DeleteState("backlog", "backlog"); err == nil {
		t.Fatal("expected same-target error")
	}

	// deleting down to the last column is refused
	single := newTestStore(t)
	if err := single.SetBoard(&Board{States: []State{{Name: "only"}}}); err != nil {
		t.Fatalf("SetBoard: %v", err)
	}
	if _, err := single.DeleteState("only", ""); err == nil {
		t.Fatal("expected last-column rejection")
	}
}

func TestUpdateState(t *testing.T) {
	s := newTestStore(t)
	disp := "Backlog!"
	color := "var(--color-board-ready)"
	elig := true
	if err := s.UpdateState("backlog", StatePatch{Display: &disp, Color: &color, Eligible: &elig}); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	st := s.Board().StateByName("backlog")
	if st.Display != disp || st.Color != color || !st.Eligible {
		t.Fatalf("update not applied: %+v", st)
	}
	// unknown state
	if err := s.UpdateState("nope", StatePatch{}); err == nil {
		t.Fatal("expected unknown-state error")
	}
}

func TestReorderStates(t *testing.T) {
	s := newTestStore(t)
	orig := s.Board().States
	names := make([]string, len(orig))
	for i, st := range orig {
		names[i] = st.Name
	}
	// reverse
	rev := make([]string, len(names))
	for i := range names {
		rev[i] = names[len(names)-1-i]
	}
	if err := s.ReorderStates(rev); err != nil {
		t.Fatalf("ReorderStates: %v", err)
	}
	got := s.Board().States
	for i := range rev {
		if got[i].Name != rev[i] {
			t.Fatalf("order[%d] = %q, want %q", i, got[i].Name, rev[i])
		}
	}
	// wrong length
	if err := s.ReorderStates(names[:len(names)-1]); err == nil {
		t.Fatal("expected wrong-length rejection")
	}
	// duplicate / unknown set
	bad := append([]string(nil), rev...)
	bad[0] = bad[1]
	if err := s.ReorderStates(bad); err == nil {
		t.Fatal("expected duplicate rejection")
	}
}

func TestStateManagementPersists(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.AddState(State{Name: "triage", Display: "Triage"}); err != nil {
		t.Fatalf("AddState: %v", err)
	}
	if _, err := s.RenameState("backlog", "todo"); err != nil {
		t.Fatalf("RenameState: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if s2.Board().StateByName("triage") == nil {
		t.Fatal("triage did not persist")
	}
	if s2.Board().StateByName("todo") == nil || s2.Board().StateByName("backlog") != nil {
		t.Fatal("rename did not persist")
	}
}
