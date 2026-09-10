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

// A session id recovered from a PAUSE is best-effort whatever the node
// declared: the CLI transcript behind it lives on the host that ran the
// node, and a human gate can outlive that host (a cloud resume gets a
// fresh pod with an empty ~/.claude). `inherit` and `fork` asked for
// continuity, not for a node that re-issues `--resume <gone>` and fails
// identically on every attempt for the rest of the run — so the engine's
// marker makes the session droppable and the executor degrades once,
// loudly, to a fresh one.
//
// A mode that takes no upstream id must stay untouched by the marker:
// there is nothing to drop, and the stamp would make a plain fresh node
// look degraded.
func TestApplySessionContinuityHonoursThePauseOptionalMarker(t *testing.T) {
	for _, c := range []struct {
		mode         ir.SessionMode
		wantOptional bool
	}{
		{ir.SessionInherit, true},
		{ir.SessionFork, true},
		{ir.SessionInheritIfAvailable, true},
		{ir.SessionPersist, true},
		{ir.SessionFresh, false},
		{ir.SessionArtifactsOnly, false},
	} {
		t.Run(c.mode.String(), func(t *testing.T) {
			e := &ClawExecutor{}
			task := &delegate.Task{}
			e.applySessionContinuity(task, backendFields{id: "worker", session: c.mode}, map[string]any{
				delegate.SessionIDKey:       "sess-from-pause",
				delegate.SessionOptionalKey: true,
			})
			if task.SessionOptional != c.wantOptional {
				t.Errorf("SessionOptional = %v, want %v", task.SessionOptional, c.wantOptional)
			}
		})
	}
}

// Without the marker, `inherit` and `fork` keep failing loudly — the
// degrade is scoped to the pause, not silently widened to every node that
// declared unconditional continuity.
func TestApplySessionContinuityKeepsInheritStrictWithoutTheMarker(t *testing.T) {
	for _, mode := range []ir.SessionMode{ir.SessionInherit, ir.SessionFork} {
		e := &ClawExecutor{}
		task := &delegate.Task{}
		e.applySessionContinuity(task, backendFields{id: "worker", session: mode}, map[string]any{
			delegate.SessionIDKey: "upstream-session",
		})
		if task.SessionOptional {
			t.Errorf("%s without the pause marker: SessionOptional = true, want false", mode)
		}
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
