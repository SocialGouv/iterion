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
//   - (true, nil)   → caller should `continue` and re-execute the same node.
//   - (false, nil)  → caller should fail terminally (failRunWithCheckpoint).
//   - (false, err)  → terminal outcome already produced (ErrRunPaused for
//     RecoveryPauseForHuman, or a context cancellation).
func (e *Engine) handleNodeFailure(ctx context.Context, rs *runState, nodeID string, execErr error) (bool, error) {
	if e.recoveryDispatch == nil {
		return false, nil
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

	emitData := map[string]interface{}{
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
	if err := e.emit(rs.runID, store.EventNodeRecovery, nodeID, emitData); err != nil {
		e.logger.Warn("recovery: failed to emit recovery event: %v", err)
	}

	if action.Kind == RecoveryCompactAndRetry {
		e.tryCompact(ctx, nodeID)
	}

	switch action.Kind {
	case RecoveryRetrySameNode, RecoveryCompactAndRetry:
		if action.Delay > 0 {
			select {
			case <-time.After(action.Delay):
			case <-ctx.Done():
				return false, e.handleContextDoneWithCheckpoint(rs, nodeID, ctx.Err())
			}
		}
		return true, nil

	case RecoveryPauseForHuman:
		reason := action.Reason
		if reason == "" {
			reason = fmt.Sprintf("recovery: %s", code)
		}
		return false, e.pauseForRecovery(rs, nodeID, code, reason, execErr)
	}

	return false, nil
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
	if err := e.emit(rs.runID, store.EventNodeStarted, nodeID, map[string]interface{}{
		"kind":            "recovery_pause",
		"recovery_code":   string(code),
		"recovery_reason": reason,
	}); err != nil {
		return err
	}
	questions := map[string]interface{}{
		"acknowledge_recovery": fmt.Sprintf("%s — resume with any answer (e.g. {\"acknowledge_recovery\": \"continue\"}) to retry.", reason),
		"recovery_code":        string(code),
	}
	eventExtra := map[string]interface{}{
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
	if err := e.doPause(rs, nodeID, questions, eventExtra, pauseInfo{}); err != nil {
		return err
	}
	return ErrRunPaused
}

// runtimeErrorFields extracts the structured fields of a *RuntimeError
// (when the error chain carries one) into a JSON-friendly map. Returns
// nil for non-RuntimeError chains so callers can omit the field.
func runtimeErrorFields(err error) map[string]interface{} {
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr == nil {
		return nil
	}
	out := map[string]interface{}{
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
