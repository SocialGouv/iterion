package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// handleNodeFailure consults the recovery dispatcher (when wired) and
// applies the resulting RecoveryAction. Returns:
//
//   - (true, code, nil)   → caller should `continue` and re-execute the same node.
//   - (false, code, nil)  → caller should fail terminally (failRunWithCheckpoint).
//   - (false, code, err)  → terminal outcome already produced (ErrRunPaused for
//     RecoveryPauseForHuman, or a context cancellation).
//
// The classified code is returned so a terminal failure carries the REAL
// cause (USAGE_LIMIT_BLOCKED, CONTEXT_LENGTH_EXCEEDED, …) instead of a
// blanket EXECUTION_FAILED. Hosts branch on it — a provider usage window
// needs a reset-aware wait, a transient blip an immediate retry — and they
// cannot tell those apart from a flattened string. An empty code means no
// dispatcher was wired, so nothing classified the failure.
func (e *Engine) handleNodeFailure(ctx context.Context, rs *runState, nodeID string, execErr error) (bool, ErrorCode, error) {
	if e.recoveryDispatch == nil {
		return false, "", nil
	}

	if rs.nodeAttempts == nil {
		rs.nodeAttempts = make(map[string]map[ErrorCode]int)
	}
	bucket, ok := rs.nodeAttempts[nodeID]
	if !ok {
		bucket = make(map[ErrorCode]int)
		rs.nodeAttempts[nodeID] = bucket
	}

	// Single dispatch call: the dispatcher classifies, asks for the
	// matching prior-attempts count via the resolver, and returns the
	// decision plus the matched code. The engine then increments the
	// bucket by 1.
	action, code := e.recoveryDispatch(ctx, execErr, func(c ErrorCode) int { return bucket[c] })
	bucket[code]++

	emitData := map[string]any{
		"code":    string(code),
		"reason":  action.Reason,
		"attempt": bucket[code],
	}
	if action.Delay > 0 {
		emitData["delay_ms"] = action.Delay.Milliseconds()
	}
	if execErr != nil {
		emitData["error"] = execErr.Error()
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeRecovery, nodeID, emitData); err != nil {
		// Best-effort — recovery proceeds even when the event store is
		// down; include code + action so the gap is debuggable.
		e.logger.Warn("recovery: failed to emit recovery event for node %q (code=%s action=%v attempt=%d): %v",
			nodeID, code, action.Kind, bucket[code], err)
	}

	if action.Kind == RecoveryCompactAndRetry {
		e.tryCompact(ctx, nodeID)
	}

	switch action.Kind {
	case RecoveryRetrySameNode, RecoveryCompactAndRetry:
		if action.Delay > 0 {
			// time.NewTimer + Stop avoids leaking the underlying
			// Timer when ctx fires before the delay elapses;
			// time.After leaves the Timer running until the delay
			// even after the parent context completes.
			timer := time.NewTimer(action.Delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return false, code, e.handleContextDoneWithCheckpoint(rs, nodeID, ctx.Err())
			}
		}
		return true, code, nil

	case RecoveryPauseForHuman:
		reason := action.Reason
		if reason == "" {
			reason = fmt.Sprintf("recovery: %s", code)
		}
		return false, code, e.pauseForRecovery(rs, nodeID, code, reason, execErr)
	}

	return false, code, nil
}

// tryCompact invokes the optional Compactor on the executor and
// swallows ErrCompactionUnsupported (architectural no-op for backends
// without a long-lived conversation handle). Real compaction errors
// log as a warning and fall through to a plain retry.
func (e *Engine) tryCompact(ctx context.Context, nodeID string) {
	c, ok := e.executor.(Compactor)
	if !ok {
		e.logger.Warn("recovery: executor does not support compaction; falling back to plain retry for node %q", nodeID)
		return
	}
	if err := c.Compact(ctx, nodeID); err != nil && !errors.Is(err, ErrCompactionUnsupported) {
		e.logger.Warn("recovery: compact failed for node %q: %v; retrying without compaction", nodeID, err)
	}
}

// pauseForRecovery synthesizes an interaction so an operator can resume
// the run after addressing the cause (rate-limit reset, budget bump,
// auth rotation). The interaction's questions field carries the
// recovery context; resume reads the answers via standard
// `iterion resume --answers-file` flow.
//
// When execErr is a *RuntimeError, its structured fields (Code, Hint,
// Cause) are preserved alongside the rendered Error() string so an
// operator inspecting the interaction or run.json sees the full
// context, not just a flattened message.
func (e *Engine) pauseForRecovery(rs *runState, nodeID string, code ErrorCode, reason string, execErr error) error {
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, nodeID, map[string]any{
		"kind":            "recovery_pause",
		"iteration":       e.currentLoopIteration(nodeID, rs.loopCounters),
		"recovery_code":   string(code),
		"recovery_reason": reason,
	}); err != nil {
		return err
	}
	questions := map[string]any{
		"acknowledge_recovery": fmt.Sprintf("%s — resume with any answer (e.g. {\"acknowledge_recovery\": \"continue\"}) to retry.", reason),
		"recovery_code":        string(code),
	}
	eventExtra := map[string]any{
		"recovery_code":   string(code),
		"recovery_reason": reason,
	}
	if execErr != nil {
		questions["last_error"] = execErr.Error()
		errFields := runtimeErrorFields(execErr)
		if len(errFields) > 0 {
			questions["last_error_details"] = errFields
			eventExtra["last_error_details"] = errFields
		}
	}
	// The checkpoint carries the recovery marker so resume re-executes the
	// node: without it the pause is indistinguishable from an ordinary
	// human gate, whose answers BECOME the node's output and whose resume
	// walks on to the next node — the failed node would be recorded as
	// finished with the acknowledgement as its result.
	info := pauseInfo{
		RecoveryPause: true,
		RecoveryCode:  code,
		Kind:          store.InteractionKindRecovery,
	}
	if err := e.doPause(rs, nodeID, questions, eventExtra, info); err != nil {
		return err
	}
	return ErrRunPaused
}

// runtimeErrorFields extracts the structured fields of a *RuntimeError
// (when the error chain carries one) into a JSON-friendly map. Returns
// nil for non-RuntimeError chains so callers can omit the field.
func runtimeErrorFields(err error) map[string]any {
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr == nil {
		return nil
	}
	out := map[string]any{
		"code":    string(rtErr.Code),
		"message": rtErr.Message,
	}
	if rtErr.NodeID != "" {
		out["node_id"] = rtErr.NodeID
	}
	if rtErr.Hint != "" {
		out["hint"] = rtErr.Hint
	}
	if rtErr.Cause != nil {
		out["cause"] = rtErr.Cause.Error()
	}
	return out
}
