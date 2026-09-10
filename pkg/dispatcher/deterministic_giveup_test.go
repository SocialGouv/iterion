package dispatcher

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// deterministicErr is a failure the shared classification says a resume
// cannot cure — the compute-expression shape of run 01a07804.
func deterministicErr() error {
	return &runtime.RuntimeError{
		Code:    store.FailureExpressionFailed,
		NodeID:  "delivery_reserve",
		Message: `compute "delivery_reserve": expr: max() takes 2 arguments, got 3`,
	}
}

// The give-up on a deterministic failure has to obey the SAME two rules the
// exhausted arm beside it obeys, because it reuses the same worker:
//
//   - it must schedule the retry entry OPTIMISTICALLY, because that entry is
//     the in-memory re-dispatch guard (isClaimed) that stops a poll tick from
//     re-picking the ticket while the give-up HTTP is in flight — and because
//     the worker's failure branch preserves it as the legacy "board can't
//     represent failed → keep retrying" fallback;
//   - it must only fire when the board can REPRESENT a terminal state.
//     `failed_state: none` maps to "" (defaultOrNone) as an explicit
//     opt-out; a give-up plan carrying "" cannot move the ticket anywhere,
//     the worker reverts it to its source state, and with no retry entry the
//     next tick re-picks it at once — an unbounded, backoff-free re-dispatch
//     loop, which is worse than the burn the arm exists to stop.
func TestFinishRun_DeterministicGiveUpKeepsTheGuardAndHonoursFailedStateNone(t *testing.T) {
	t.Run("representable failed state: optimistic guard then the terminal move", func(t *testing.T) {
		ft := newStateAwareTracker()
		ft.add(tracker.Issue{ID: "fake:d1", Identifier: "fake#d1", Title: "go", WorkflowState: "in_progress"})
		c, wsDir := newStateTestDispatcher(t, &StubRunner{}, ft, time.Hour, "in_progress")
		cfg := c.cfg.Load()
		cfg.Agent.FailedState = "blocked"
		// No attempt ceiling: the arm must fire on the FIRST failure, not
		// because the ladder ran out.
		cfg.Agent.MaxAttempts = 0
		c.cfg.Store(cfg)

		c.state.running["fake:d1"] = &runningEntry{
			IssueID: "fake:d1", Identifier: "fake#d1", RunID: "run-d1",
			WorkflowState: "in_progress",
			WorkspacePath: filepath.Join(wsDir, "fake_d1"),
			StartedAt:     time.Now(), TransitionedFromState: "ready",
		}

		c.finishRun(context.Background(), "fake:d1", deterministicErr())

		if _, ok := c.state.retries["fake:d1"]; !ok {
			t.Fatal("no optimistic retry guard — a poll tick can re-pick the ticket while the give-up HTTP is in flight, and the worker's failure branch has nothing to preserve")
		}
		c.workersWG.Wait()
		applyNextCmd(t, c, context.Background())

		if got := ft.issueState("fake:d1"); got != "blocked" {
			t.Fatalf("issue state = %q, want blocked — the deterministic verdict must be visible on the board", got)
		}
		if _, ok := c.state.retries["fake:d1"]; ok {
			t.Fatal("retry guard not dropped after a successful give-up move — the ticket would retry a failure that cannot change")
		}
	})

	t.Run("failed_state: none falls through to the bounded ladder", func(t *testing.T) {
		ft := newStateAwareTracker()
		ft.add(tracker.Issue{ID: "fake:d2", Identifier: "fake#d2", Title: "go", WorkflowState: "in_progress"})
		c, wsDir := newStateTestDispatcher(t, &StubRunner{}, ft, time.Hour, "in_progress")
		cfg := c.cfg.Load()
		cfg.Agent.FailedState = "" // what `failed_state: none` resolves to
		cfg.Agent.MaxAttempts = 0
		c.cfg.Store(cfg)

		c.state.running["fake:d2"] = &runningEntry{
			IssueID: "fake:d2", Identifier: "fake#d2", RunID: "run-d2",
			WorkflowState: "in_progress",
			WorkspacePath: filepath.Join(wsDir, "fake_d2"),
			StartedAt:     time.Now(), TransitionedFromState: "ready",
		}

		c.finishRun(context.Background(), "fake:d2", deterministicErr())

		if _, ok := c.state.retries["fake:d2"]; !ok {
			t.Fatal("no retry entry — with no terminal state to move to, the ticket must keep the BOUNDED ladder, not become instantly re-pickable")
		}
		c.workersWG.Wait()

		// Reverted to its source state, never moved to the empty one.
		if got := ft.issueState("fake:d2"); got != "ready" {
			t.Fatalf("issue state = %q, want ready — the operator opted out of a terminal state, so the ticket reverts", got)
		}
	})
}
