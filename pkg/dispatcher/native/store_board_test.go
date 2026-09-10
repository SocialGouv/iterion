package native

import "testing"

// TestDeleteState_TerminalMigrationHonoursDependents: deleting a terminal
// column with a working-state target reopens every card in it at once.
// Reopen refuses that for a card whose completion already promoted
// dependents; the bulk gesture must not be the way around that refusal.
func TestDeleteState_TerminalMigrationHonoursDependents(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	blocker, err := s.Create(Issue{Title: "blocker", State: StateReady})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	if _, err := s.SetState(blocker.ID, StateDone); err != nil {
		t.Fatalf("SetState done: %v", err)
	}
	dep, err := s.Create(Issue{Title: "dependent", State: StateReady, Blockers: []string{blocker.ID}})
	if err != nil {
		t.Fatalf("Create dependent: %v", err)
	}
	// The single-card gesture is refused...
	if _, err := s.Reopen(blocker.ID, StateReady); err == nil {
		t.Fatal("Reopen with a promoted dependent must be refused (precondition)")
	}
	// ...so the bulk one must be too.
	if _, err := s.DeleteState(StateDone, StateReady); err == nil {
		t.Fatal("deleting a terminal column into a working one is a bulk reopen: it must honour the same dependents check")
	}
	if cur, _ := s.Get(blocker.ID); cur.State != StateDone {
		t.Fatalf("the refused migration must leave the card terminal, got %q", cur.State)
	}
	// With the dependency cleared, the migration proceeds.
	if _, err := s.Update(dep.ID, Patch{Blockers: &[]string{}}); err != nil {
		t.Fatalf("clear blockers: %v", err)
	}
	if n, err := s.DeleteState(StateDone, StateReady); err != nil || n != 1 {
		t.Fatalf("DeleteState after clearing dependents = (%d, %v), want 1 card migrated", n, err)
	}
}
