package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Run failure
// ---------------------------------------------------------------------------

// failRun marks a run as failed and emits the run_failed event.
// If reason is already a RuntimeError it preserves the code and hint.
func (e *Engine) failRun(ctx context.Context, runID, nodeID, reason string) error {
	return e.failRunWithCode(ctx, runID, nodeID, reason, ErrCodeExecutionFailed, "")
}

// failRunErr marks a run as failed, preserving a structured error if present.
// Store/event errors are propagated so callers know whether the failure was persisted.
func (e *Engine) failRunErr(ctx context.Context, runID, nodeID string, origErr error) error {
	var rtErr *RuntimeError
	if errors.As(origErr, &rtErr) {
		if storeErr := e.store.UpdateRunStatus(ctx, runID, store.RunStatusFailed, rtErr.Message); storeErr != nil {
			e.logger.Error("failed to persist run failure status: %v", storeErr)
			return fmt.Errorf("runtime: node %q failed (%s) and could not persist failure: %w", nodeID, rtErr.Message, storeErr)
		}
		if err := e.emit(ctx, runID, store.EventRunFailed, nodeID, map[string]any{
			"error": rtErr.Message,
			"code":  string(rtErr.Code),
		}); err != nil {
			e.logger.Warn("failed to emit run_failed event: %v", err)
		}
		if rtErr.NodeID == "" {
			rtErr.NodeID = nodeID
		}
		return rtErr
	}
	return e.failRun(ctx, runID, nodeID, origErr.Error())
}

// failRunWithCode marks a run as failed and returns a structured RuntimeError.
// If the store update fails, the store error is returned instead of the runtime
// error so callers know the failure state was not persisted.
func (e *Engine) failRunWithCode(ctx context.Context, runID, nodeID, reason string, code ErrorCode, hint string) error {
	if storeErr := e.store.UpdateRunStatus(ctx, runID, store.RunStatusFailed, reason); storeErr != nil {
		e.logger.Error("failed to persist run failure status: %v", storeErr)
		return fmt.Errorf("runtime: node %q failed (%s) and could not persist failure: %w", nodeID, reason, storeErr)
	}
	if err := e.emit(ctx, runID, store.EventRunFailed, nodeID, map[string]any{
		"error": reason,
		"code":  string(code),
	}); err != nil {
		e.logger.Warn("failed to emit run_failed event: %v", err)
	}
	return &RuntimeError{
		Code:    code,
		Message: reason,
		NodeID:  nodeID,
		Hint:    hint,
	}
}

// ---------------------------------------------------------------------------
// Resumable failure — checkpoint-aware variants
// ---------------------------------------------------------------------------

// emitRunFailedAndReturn emits a run_failed event with the resumable
// flag set (best-effort: a store-side failure is logged at warn, not
// returned, since the run-failure path itself is already best-effort)
// and returns the matching RuntimeError. Shared by every checkpoint-
// aware failure path so the "what does a resumable failure look like"
// decision lives in one place.
func (e *Engine) emitRunFailedAndReturn(ctx context.Context, runID, nodeID, reason string, code ErrorCode) error {
	if err := e.emit(ctx, runID, store.EventRunFailed, nodeID, map[string]any{
		"error":     reason,
		"code":      string(code),
		"resumable": true,
	}); err != nil {
		e.logger.Warn("failed to emit run_failed event: %v", err)
	}
	return &RuntimeError{Code: code, Message: reason, NodeID: nodeID}
}

// failRunDeliberate handles the DSL FailNode: the graph routed here on
// purpose, so the run is terminal — status failed, no auto-resume — but
// the checkpoint is preserved. The state on disk is coherent (the fail
// node is reached from a normal node boundary) and an explicit operator
// rewind remains possible, same as a cancelled run. Falls back to a
// plain checkpoint-less failure if the write fails.
func (e *Engine) failRunDeliberate(rs *runState, nodeID, reason string) error {
	cp := buildCheckpoint(rs, nodeID)
	if storeErr := e.store.FailRunTerminal(rs.ctx, rs.runID, cp, reason); storeErr != nil {
		e.logger.Error("failed to persist terminal failure: %v", storeErr)
		return e.failRun(rs.ctx, rs.runID, nodeID, reason)
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventRunFailed, nodeID, map[string]any{
		"error":      reason,
		"code":       string(ErrCodeExecutionFailed),
		"rewindable": true,
	}); err != nil {
		e.logger.Warn("failed to emit run_failed event: %v", err)
	}
	return &RuntimeError{
		Code:    ErrCodeExecutionFailed,
		Message: reason,
		NodeID:  nodeID,
	}
}

// failRunWithCheckpoint marks a run as failed_resumable with a checkpoint,
// enabling resume from the last completed node. Falls back to a regular
// (non-resumable) failure if the checkpoint write fails.
func (e *Engine) failRunWithCheckpoint(rs *runState, nodeID, reason string) error {
	e.captureFailureBoundary(rs, nodeID)
	cp := buildCheckpoint(rs, nodeID)
	if storeErr := e.store.FailRunResumable(rs.ctx, rs.runID, cp, reason); storeErr != nil {
		e.logger.Error("failed to persist resumable failure: %v", storeErr)
		return e.failRun(rs.ctx, rs.runID, nodeID, reason)
	}
	return e.emitRunFailedAndReturn(rs.ctx, rs.runID, nodeID, reason, ErrCodeExecutionFailed)
}

// failRunErrWithCheckpoint is the checkpoint-aware variant of failRunErr.
func (e *Engine) failRunErrWithCheckpoint(rs *runState, nodeID string, origErr error) error {
	var rtErr *RuntimeError
	if errors.As(origErr, &rtErr) {
		// Only on this branch: the fallthrough below goes through
		// failRunWithCheckpoint, which captures for itself.
		e.captureFailureBoundary(rs, nodeID)
		cp := buildCheckpoint(rs, nodeID)
		if storeErr := e.store.FailRunResumable(rs.ctx, rs.runID, cp, rtErr.Message); storeErr != nil {
			e.logger.Error("failed to persist resumable failure: %v", storeErr)
			return e.failRunErr(rs.ctx, rs.runID, nodeID, origErr)
		}
		// Preserve the original *RuntimeError identity so callers can
		// errors.As back to the same value; the helper would otherwise
		// allocate a fresh one.
		if err := e.emit(rs.ctx, rs.runID, store.EventRunFailed, nodeID, map[string]any{
			"error":     rtErr.Message,
			"code":      string(rtErr.Code),
			"resumable": true,
		}); err != nil {
			e.logger.Warn("failed to emit run_failed event: %v", err)
		}
		if rtErr.NodeID == "" {
			rtErr.NodeID = nodeID
		}
		return rtErr
	}
	return e.failRunWithCheckpoint(rs, nodeID, origErr.Error())
}

// ---------------------------------------------------------------------------
// Context handling
// ---------------------------------------------------------------------------

// handleContextDoneWithCheckpoint handles context cancellation or deadline
// exceeded, preserving the checkpoint so the run can be resumed. rs.ctx is
// already done when we get here, so we detach for the store writes using
// context.WithoutCancel — values (tenant/user identity for cloud mode) are
// preserved but cancellation isn't. Bounded by a 5s timeout, well under the
// typical k8s pod-termination grace period.
func (e *Engine) handleContextDoneWithCheckpoint(rs *runState, nodeID string, ctxErr error) error {
	// An operator cancel and a runner drain both stop the run INSIDE a
	// node, so they leave the same unrecorded production a failure does —
	// and `cancelled` is a rewindable status, so the rewind that follows
	// needs the boundary just as much. Inline, ahead of the status write:
	// see captureStopBoundary on why the order is that way round.
	e.captureFailureBoundary(rs, nodeID)
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(rs.ctx), 5*time.Second)
	defer cancel()

	if errors.Is(ctxErr, context.Canceled) {
		cp := buildCheckpoint(rs, nodeID)
		// Infrastructure interruption (runner drain / lost heartbeat): the
		// caller cancelled with ErrRunInterrupted as the cause. This is NOT
		// an operator cancel — write failed_resumable + a resumable
		// run_failed event so the run auto-resumes on a healthy pod, and
		// return ErrRunInterrupted so the runner naks (not acks) it. Writing
		// the terminal status here (once, correctly) is why no downstream
		// CAS or event-suppression is needed.
		if errors.Is(context.Cause(rs.ctx), ErrRunInterrupted) {
			reason := fmt.Sprintf("interrupted at node %s (resumable)", nodeID)
			if storeErr := e.store.FailRunResumable(storeCtx, rs.runID, cp, reason); storeErr != nil {
				e.logger.Error("failed to persist resumable interruption: %v", storeErr)
				return e.failRun(storeCtx, rs.runID, nodeID, reason)
			}
			if err := e.emit(storeCtx, rs.runID, store.EventRunFailed, nodeID, map[string]any{
				"error":       reason,
				"code":        string(ErrCodeExecutionFailed),
				"resumable":   true,
				"interrupted": true,
			}); err != nil {
				e.logger.Warn("failed to emit run_failed event: %v", err)
			}
			return fmt.Errorf("%w: at node %s", ErrRunInterrupted, nodeID)
		}
		// Operator cancel (or plain SIGINT): terminal cancelled.
		if err := e.store.SaveCheckpoint(storeCtx, rs.runID, cp); err != nil {
			e.logger.Error("failed to save checkpoint on cancellation: %v", err)
		}
		// CAS, not a blind write: in cloud mode the publisher flips the doc to
		// cancelled WITH a specific reason ("superseded by a newer delivery…",
		// "cancelled by user") BEFORE the cancel subject reaches this engine.
		// An unconditional write here would overwrite that reason with the
		// generic "run cancelled" — which is then what the run list, board
		// cards and merge-gate synthetic statuses display. Skip the write when
		// the run is already terminal; the reason recorded first is the truth.
		cancellable := []store.RunStatus{
			store.RunStatusRunning,
			store.RunStatusPausedWaitingHuman,
			store.RunStatusPausedOperator,
			store.RunStatusFailedResumable,
		}
		if changed, err := e.store.UpdateRunStatusIf(storeCtx, rs.runID, store.RunStatusCancelled, "run cancelled", cancellable); err != nil {
			e.logger.Error("failed to persist cancellation status: %v", err)
		} else if !changed {
			e.logger.Debug("run %s already terminal — keeping the recorded cancellation reason", rs.runID)
		}
		if err := e.emit(storeCtx, rs.runID, store.EventRunCancelled, nodeID, map[string]any{
			"reason": "context cancelled",
		}); err != nil {
			e.logger.Warn("failed to emit run_cancelled event: %v", err)
		}
		return fmt.Errorf("%w: interrupted at node %s", ErrRunCancelled, nodeID)
	}
	// context.DeadlineExceeded → save checkpoint and mark as resumable.
	// Inline the failRunWithCheckpoint logic so it runs on storeCtx, not
	// the already-expired rs.ctx.
	reason := fmt.Sprintf("timeout: %s", ctxErr.Error())
	cp := buildCheckpoint(rs, nodeID)
	if storeErr := e.store.FailRunResumable(storeCtx, rs.runID, cp, reason); storeErr != nil {
		e.logger.Error("failed to persist resumable failure: %v", storeErr)
		return e.failRun(storeCtx, rs.runID, nodeID, reason)
	}
	return e.emitRunFailedAndReturn(storeCtx, rs.runID, nodeID, reason, ErrCodeExecutionFailed)
}

// wrapContextErr wraps a context error for branch-level reporting.
func (e *Engine) wrapContextErr(ctxErr error) error {
	if errors.Is(ctxErr, context.Canceled) {
		return fmt.Errorf("%w: %v", ErrRunCancelled, ctxErr)
	}
	return ctxErr
}
