package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// failSpentBudgetBeforeResume applies the same persisted accounting and cap
// raises as a rebuilt runState, but does so immediately after the resume CAS
// and before restoreRunEnv/startSandbox. A redelivery that cannot start its
// next node therefore reaches the normal budget_exceeded + run_failed
// disposition without provisioning a sandbox merely to rediscover the cap.
func (e *Engine) failSpentBudgetBeforeResume(ctx context.Context, r *store.Run) error {
	if e == nil || e.workflow == nil || r == nil || r.Checkpoint == nil {
		return nil
	}
	b := newSharedBudget(e.workflow.Budget, e.logger)
	if b == nil {
		return nil
	}
	cp := r.Checkpoint
	b.Restore(cp.BudgetTokensUsed, cp.BudgetCostUSD, cp.BudgetIterationsUsed,
		time.Duration(cp.BudgetElapsedNS), cp.BudgetUnpricedTokens, cp.BudgetUnpricedNodes)
	if raises := r.BudgetRaises; raises != nil {
		b.RaiseCaps(ir.BudgetOverrides{
			MaxCostUSD:    raises.MaxCostUSD,
			MaxTokens:     raises.MaxTokens,
			MaxIterations: raises.MaxIterations,
			MaxDuration:   raises.MaxDuration,
		})
	}

	checks := b.Check()
	if exc := findExceeded(checks); exc != nil {
		// A run already inside its bounded exit allowance must take the same
		// path as an uninterrupted run. Let resume rebuild its state and reach
		// checkBudgetBeforeExec, which grants and audits the grace while the
		// loop guard prevents another iteration from starting.
		if _, ok := e.withinSharedBudgetGrace(b); ok {
			return nil
		}
		return e.finalizeSpentBudgetBeforeResume(ctx, r, exc, false)
	}
	// The in-run pre-exec gate refuses a new node at 90% to reserve room for
	// concurrent overage. Reproduce that decision here too: provisioning a
	// sandbox cannot change the accounting and would immediately hit the same
	// hard-limit branch.
	if hard := findHardLimited(checks); hard != nil {
		return e.finalizeSpentBudgetBeforeResume(ctx, r, hard, true)
	}
	return nil
}

func (e *Engine) finalizeSpentBudgetBeforeResume(ctx context.Context, r *store.Run, check *budgetCheckResult, hardLimit bool) error {
	nodeID := r.Checkpoint.NodeID
	data := map[string]any{
		"dimension": check.dimension,
		"used":      check.used,
		"limit":     check.limit,
	}
	var rtErr *RuntimeError
	if hardLimit {
		data["hard_limit"] = true
		rtErr = &RuntimeError{
			Code:    ErrCodeBudgetExceeded,
			Message: fmt.Sprintf("budget hard limit reached: %s at %.0f%% (%.0f/%.0f)", check.dimension, (check.used/check.limit)*100, check.used, check.limit),
			NodeID:  nodeID,
			Hint:    fmt.Sprintf("increase the %s budget or optimize the workflow; new executions are blocked at 90%% to prevent concurrent overage", check.dimension),
			Cause:   ErrBudgetExceeded,
		}
	} else {
		rtErr = &RuntimeError{
			Code:    ErrCodeBudgetExceeded,
			Message: fmt.Sprintf("budget exceeded: %s (%.0f/%.0f)", check.dimension, check.used, check.limit),
			NodeID:  nodeID,
			Hint:    fmt.Sprintf("raise budget.%s and resume — local: `iterion resume --max-%s`; cloud: `runs resume --file <workflow with the raised budget>`", check.dimension, check.dimension),
			Cause:   ErrBudgetExceeded,
		}
	}

	if err := e.emit(ctx, r.ID, store.EventBudgetExceeded, nodeID, data); err != nil && e.logger != nil {
		e.logger.Warn("failed to emit budget_exceeded event: %v", err)
	}
	// The run was claimed to running immediately before this guard. Restore
	// its existing rich checkpoint while moving it back to failed_resumable,
	// exactly like failRunErrWithCheckpoint does after an in-loop budget death.
	if err := e.store.FailRunResumable(ctx, r.ID, r.Checkpoint, rtErr.Message, rtErr.Code); err != nil {
		if e.logger != nil {
			e.logger.Error("failed to persist pre-resume budget failure: %v", err)
		}
		return e.failRunErr(ctx, r.ID, nodeID, rtErr)
	}
	if err := e.emit(ctx, r.ID, store.EventRunFailed, nodeID, map[string]any{
		"error":     rtErr.Message,
		"code":      string(rtErr.Code),
		"resumable": true,
	}); err != nil && e.logger != nil {
		e.logger.Warn("failed to emit run_failed event: %v", err)
	}
	return rtErr
}
