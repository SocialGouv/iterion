package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// pauseOperatorWithCheckpoint is the shared soft-pause path: it saves a
// checkpoint anchored at nodeID (the node about to execute), flips the
// run status to paused_operator (distinct from paused_waiting_human so
// the studio can render a banner and resume routes through the
// cancellation-style restore path, not the human-answers path), and
// emits a run_paused event whose "reason" + extra fields the caller
// supplies via data. Returns ErrRunPausedOperator wrapped with detail —
// reusing that sentinel means every resumable-pause handler (runner,
// resume dispatch, dispatcher retry) treats both operator and cost-cap
// pauses correctly with no extra wiring.
func (e *Engine) pauseOperatorWithCheckpoint(rs *runState, nodeID, reason, detail string, data map[string]any) error {
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(rs.ctx), 5*time.Second)
	defer cancel()

	cp := buildCheckpoint(rs, nodeID)
	if err := e.store.SaveCheckpoint(storeCtx, rs.runID, cp); err != nil {
		e.logger.Error("failed to save checkpoint on %s pause: %v", reason, err)
	}
	if err := e.store.UpdateRunStatus(storeCtx, rs.runID, store.RunStatusPausedOperator, ""); err != nil {
		e.logger.Error("failed to persist %s pause status: %v", reason, err)
	}
	if data == nil {
		data = make(map[string]any, 1)
	}
	data["reason"] = reason
	if err := e.emit(storeCtx, rs.runID, store.EventRunPaused, nodeID, data); err != nil {
		e.logger.Warn("failed to emit run_paused event: %v", err)
	}
	if detail != "" {
		return fmt.Errorf("%w: paused at node %s (%s)", ErrRunPausedOperator, nodeID, detail)
	}
	return fmt.Errorf("%w: paused at node %s", ErrRunPausedOperator, nodeID)
}

// handleOperatorPauseWithCheckpoint is called from execLoop when the
// engine's WithPauseSignal channel fires (the studio "Pause now" button
// / POST /api/runs/{id}/pause).
func (e *Engine) handleOperatorPauseWithCheckpoint(rs *runState, nodeID string) error {
	return e.pauseOperatorWithCheckpoint(rs, nodeID, "operator", "", nil)
}

// handleCostCapPause pauses a run because the shared per-day spend cap is
// over the limit. It tags the run_paused event with reason=cost_cap_daily
// and the spend numbers so the studio can render the cost-cap banner. The
// run auto-resumes once the operator overrides for the day or the UTC day
// rolls over and the ledger resets.
func (e *Engine) handleCostCapPause(rs *runState, nodeID string, st CapStatus) error {
	e.logger.Warn("run %s paused: daily spend cap reached ($%.2f >= $%.2f for %s)",
		rs.runID, st.SpentUSD, st.LimitUSD, st.Date)
	return e.pauseOperatorWithCheckpoint(rs, nodeID, CapReasonDaily,
		fmt.Sprintf("daily spend cap $%.2f >= $%.2f", st.SpentUSD, st.LimitUSD),
		map[string]any{
			"spent_usd": st.SpentUSD,
			"limit_usd": st.LimitUSD,
			"date":      st.Date,
		})
}

// handleBudgetCapPause pauses a run whose declared budget policy is
// `on_exceeded: pause` at the boundary the cap crossing reached — instead
// of the fail the default policy ends the run with. The run_paused event
// carries reason=budget_cap_run and the dimension's numbers; an operator
// raise (`raise-budget`) resumes it at the preserved checkpoint, and the
// resume's own budget preflight re-judges the raised cap.
func (e *Engine) handleBudgetCapPause(rs *runState, nodeID string, exc *budgetCheckResult) error {
	e.logger.Warn("run %s paused: budget %s cap reached (%.0f/%.0f), policy pause",
		rs.runID, exc.dimension, exc.used, exc.limit)
	// paused_operator is a rewindable status: capture the workspace before
	// the durable write opens the pause interval, so an `iterion rewind`
	// on a budget park sees the same gap the fail path's
	// captureFailureBoundary records — at the deadline kill the node HAS
	// run and may have left files nothing else records.
	e.capturePauseBoundary(rs, nodeID)
	return e.pauseOperatorWithCheckpoint(rs, nodeID, "budget_cap_run",
		fmt.Sprintf("budget %s cap reached (%.0f/%.0f)", exc.dimension, exc.used, exc.limit),
		map[string]any{
			"dimension":  exc.dimension,
			"used":       exc.used,
			"limit":      exc.limit,
			"hard_limit": exc.hardLimited,
		})
}
