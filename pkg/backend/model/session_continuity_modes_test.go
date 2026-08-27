package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestApplySessionContinuityMarksOptionalModes pins the three lines that
// decide whether a node's session is droppable at all. Without them the
// executor's degrade is unreachable and branch-improve-loop's plan_revise
// returns to wedging on a dead session across a cloud resume — every
// other test in the tree hand-builds delegate.Task{SessionOptional: …},
// so a refactor that dropped or narrowed the mapping stayed green
// (R552e44).
func TestApplySessionContinuityMarksOptionalModes(t *testing.T) {
	cases := []struct {
		mode         ir.SessionMode
		wantID       bool // does the mode inherit the upstream id at all?
		wantOptional bool
		wantFork     bool
	}{
		// Best-effort: the id resolved, but its backing state may be gone.
		{ir.SessionInheritIfAvailable, true, true, false},
		{ir.SessionPersist, true, true, false},
		// Unconditional continuity: a failure keeps failing loudly.
		{ir.SessionInherit, true, false, false},
		{ir.SessionFork, true, false, true},
		// Modes that carry no upstream session at all.
		{ir.SessionFresh, false, false, false},
		{ir.SessionArtifactsOnly, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.mode.String(), func(t *testing.T) {
			e := &ClawExecutor{}
			task := &delegate.Task{}
			e.applySessionContinuity(task, backendFields{id: "plan_revise", session: c.mode}, map[string]any{
				"_session_id":          "upstream-session",
				"_session_fingerprint": "anthropic-direct",
			})
			if got := task.SessionID != ""; got != c.wantID {
				t.Fatalf("SessionID set = %v, want %v", got, c.wantID)
			}
			if task.SessionOptional != c.wantOptional {
				t.Errorf("SessionOptional = %v, want %v", task.SessionOptional, c.wantOptional)
			}
			if task.ForkSession != c.wantFork {
				t.Errorf("ForkSession = %v, want %v", task.ForkSession, c.wantFork)
			}
		})
	}
}

// A best-effort mode with NO upstream id must not claim a droppable
// session: there is nothing to drop, and the stamp would make a plain
// fresh node look degraded.
func TestApplySessionContinuityNoIDLeavesOptionalClear(t *testing.T) {
	for _, mode := range []ir.SessionMode{ir.SessionInheritIfAvailable, ir.SessionPersist} {
		e := &ClawExecutor{}
		task := &delegate.Task{}
		e.applySessionContinuity(task, backendFields{id: "plan_revise", session: mode}, map[string]any{})
		if task.SessionOptional || task.SessionID != "" {
			t.Errorf("%s with no upstream id: SessionOptional=%v SessionID=%q, want clear",
				mode, task.SessionOptional, task.SessionID)
		}
	}
}
