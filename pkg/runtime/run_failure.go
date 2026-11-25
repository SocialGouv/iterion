package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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

// persistCtx is the context a run's failure is written on: the run's own —
// or, for an undecided effect once that context is done, a detached one,
// bounded, keeping its values (tenant identity) and not its cancellation. An
// undecided effect keeps its own code past a cancel, and a store that honours
// a done context would refuse it: the run would stay `running`, and the runner
// would redeliver it. Any other failure on a done context keeps the cancel's
// own handling — the status the publisher wrote, the drain's redelivery.
func persistCtx(ctx context.Context, err error) (context.Context, context.CancelFunc) {
	if terminationScope(ctx) != nil || ctx.Err() == nil || !IsAmbiguousEffect(err) {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// recordedCode is the code a failure is recorded under: an undecided effect
// anywhere in its chain outranks the code a wrapper gave it — a subbot
// boundary, the catch-all default — because every other code is one that
// something re-drives, and a re-drive may duplicate the effect.
func recordedCode(rtErr *RuntimeError, err error) ErrorCode {
	if IsAmbiguousEffect(err) {
		return ErrCodeAmbiguousEffect
	}
	return rtErr.Code
}

// unpersisted is what a failure whose status write failed returns: the store
// error alone, so the caller knows the failure was not recorded — and, for an
// undecided effect, the runtime error beside it, whose code keeps a caller
// from re-driving the run.
func unpersisted(rtErr *RuntimeError, nodeID, message string, storeErr error) error {
	err := fmt.Errorf("runtime: node %q failed (%s) and could not persist failure: %w", nodeID, message, storeErr)
	if rtErr.Code == ErrCodeAmbiguousEffect {
		return errors.Join(rtErr, err)
	}
	return err
}

// failRunErr marks a run as failed, preserving a structured error if present.
// Store/event errors are propagated so callers know whether the failure was persisted.
func (e *Engine) failRunErr(ctx context.Context, runID, nodeID string, origErr error) error {
	if errors.Is(origErr, errCorrectionOwnershipLost) {
		return origErr
	}
	if store.IsValueFormatRefusal(origErr) {
		return origErr
	}
	ctx, cancel := persistCtx(ctx, origErr)
	defer cancel()
	var rtErr *RuntimeError
	if errors.As(origErr, &rtErr) {
		rtErr.Code = recordedCode(rtErr, origErr)
		remedy := RemedyOf(rtErr)
		if storeErr := e.recordFailureStatus(ctx, runID, store.RunStatusFailed, remedy.Annotate(rtErr.Message), rtErr.Code); storeErr != nil {
			e.logger.Error("failed to persist run failure status: %v", storeErr)
			return unpersisted(rtErr, nodeID, rtErr.Message, storeErr)
		}
		data := map[string]any{
			"error": rtErr.Message,
			"code":  string(rtErr.Code),
		}
		remedy.Record(data)
		if err := e.emit(ctx, runID, store.EventRunFailed, nodeID, data); err != nil {
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
	ctx, cancel := persistCtx(ctx, nil)
	defer cancel()
	rtErr := &RuntimeError{
		Code:    code,
		Message: reason,
		NodeID:  nodeID,
		Hint:    hint,
	}
	remedy := RemedyOf(rtErr)
	if storeErr := e.recordFailureStatus(ctx, runID, store.RunStatusFailed, remedy.Annotate(reason), code); storeErr != nil {
		e.logger.Error("failed to persist run failure status: %v", storeErr)
		return unpersisted(rtErr, nodeID, reason, storeErr)
	}
	data := map[string]any{
		"error": reason,
		"code":  string(code),
	}
	remedy.Record(data)
	if err := e.emit(ctx, runID, store.EventRunFailed, nodeID, data); err != nil {
		e.logger.Warn("failed to emit run_failed event: %v", err)
	}
	return rtErr
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

// failOutcome is the single place a fail node's diagnosis is derived, so
// the trunk and the fan-out branch path cannot disagree about what the
// operator is told. An undeclared field falls back to the engine's own
// constant — the untyped `-> fail` target keeps behaving exactly as it
// always has.
//
// The message is resolved against the run's namespaces here, at fail
// time: what a bot wants to say about WHY it refused is usually the very
// figure that made it refuse.
func (e *Engine) failOutcome(rs *runState, n *ir.FailNode) failNodeOutcome {
	out := failNodeOutcome{
		reason:    deliberateFailReason,
		code:      store.FailureFailNode,
		resumable: n != nil && n.Resumable,
	}
	if n == nil {
		return out
	}
	if n.Code != "" {
		out.code = store.FailureCode(n.Code)
	}
	if n.Message != nil {
		if rendered := renderMappingValue(e.resolveMapping(n.Message, rs.scope())); rendered != "" {
			out.reason = rendered
		}
	}
	return out
}

// deliberateFailReason is the wording a fail node with no `message:`
// still produces — the historical text, kept because operator greps and
// tests key on it.
const deliberateFailReason = "workflow reached fail node"

// failNodeOutcome is what reaching a fail node puts ON THE RUN: the
// operator-facing reason, the machine-readable code, and whether the run
// stays resumable.
type failNodeOutcome struct {
	reason    string
	code      store.FailureCode
	resumable bool
}

// resumableFailAnchor returns the node a `resumable: true` fail node's
// checkpoint must be anchored on: the PREDECESSOR whose outgoing edge
// routed into it.
//
// Anchoring on the fail node itself would make the promise a lie.
// resumeFromFailure starts execLoop at cp.NodeID, so a checkpoint on the
// fail node re-dispatches the fail node, which fails again identically —
// the guard that refused is never re-evaluated, and the operator's raised
// cap changes nothing. The predecessor is the guard: re-executing it is
// what lets the run take the other edge this time.
//
// Reported false (with a reason) when no single predecessor can be named:
//
//   - the fail node IS the workflow entry, so nothing routed into it;
//   - several branches converged on it, so no ONE guard owns the refusal.
//
// The caller then keeps the run TERMINAL and says why, rather than
// persisting a resumable checkpoint whose only possible resume is a
// no-op. A fan-out branch that reaches a fail node never arrives here at
// all — branch.go carries the outcome as a branch error and the collector
// decides.
func (e *Engine) resumableFailAnchor(rs *runState, failNodeID string) (anchor, reason string) {
	if rs == nil {
		return "", "no run state"
	}
	selected := rs.selectedIncoming[failNodeID]
	switch len(selected) {
	case 0:
		return "", "nothing routed into it (it is the workflow entry, or its incoming edge was not recorded)"
	case 1:
	default:
		return "", fmt.Sprintf("%d branches converged on it, so no single guard owns the refusal", len(selected))
	}

	from := selected[0].From
	if from == "" {
		return "", "the recorded incoming edge names no source"
	}
	node, ok := e.workflow.Nodes[from]
	if !ok {
		return "", fmt.Sprintf("its predecessor %q is not in the workflow", from)
	}
	// A terminal predecessor is impossible by construction (a terminal has
	// no outgoing edges) — guarded because resuming INTO one would end the
	// run without re-deciding anything, which is the same no-op by another
	// route.
	if ir.IsTerminalNode(node) {
		return "", fmt.Sprintf("its predecessor %q is itself a terminal node", from)
	}
	return from, ""
}

// failRunDeliberate handles the DSL FailNode: the graph routed here on
// purpose, so the run is terminal by default — status failed, no
// auto-resume — but the checkpoint is preserved. The state on disk is
// coherent (the fail node is reached from a normal node boundary) and an
// explicit operator rewind remains possible, same as a cancelled run.
//
// A node that declares `resumable: true` parks failed_resumable instead:
// a budget guard whose cure is "raise the cap and continue" must not make
// the operator re-pay the phase it already completed. Its checkpoint is
// anchored on the GUARD, not on the fail node — see resumableFailAnchor.
// Falls back to a plain checkpoint-less failure if the write fails.
func (e *Engine) failRunDeliberate(rs *runState, nodeID string, outcome failNodeOutcome) error {
	anchor := nodeID
	persist := e.recordFailureTerminal
	if outcome.resumable {
		guard, why := e.resumableFailAnchor(rs, nodeID)
		if guard == "" {
			// The declaration cannot be honoured. Say so at WARN and end
			// the run terminal: a resumable checkpoint here would only
			// ever replay this same refusal, which reads to the operator
			// as a resume that silently does nothing.
			e.logger.Warn("runtime: fail node %q declares `resumable: true` but the run cannot resume from it — %s; ending the run terminal (failed) instead", nodeID, why)
			outcome.resumable = false
		} else {
			anchor = guard
			e.captureFailureBoundary(rs, nodeID)
			persist = e.recordFailureResumable
		}
	}
	cp := buildCheckpoint(rs, anchor)
	if storeErr := persist(rs.ctx, rs.runID, cp, outcome.reason, outcome.code); storeErr != nil {
		e.logger.Error("failed to persist deliberate failure: %v", storeErr)
		return e.failRun(rs.ctx, rs.runID, nodeID, outcome.reason)
	}
	data := map[string]any{
		"error": outcome.reason,
		"code":  string(outcome.code),
	}
	if outcome.resumable {
		data["resumable"] = true
	} else {
		// A terminal deliberate failure is still rewindable: the
		// checkpoint is intact, so an operator may re-anchor and resume
		// by hand. The flag is what tells the studio to offer it.
		data["rewindable"] = true
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventRunFailed, nodeID, data); err != nil {
		e.logger.Warn("failed to emit run_failed event: %v", err)
	}
	return &RuntimeError{
		Code:    outcome.code,
		Message: outcome.reason,
		NodeID:  nodeID,
		// The sentinel is what tells a CLOUD runner this failure is a
		// decision, not a fault: the code alone is bot-defined, so no
		// allow-list can recognise it, and a redelivery re-runs the same
		// guard against the same inputs to the same verdict. See
		// ErrDeliberateFailure.
		Cause: ErrDeliberateFailure,
	}
}

// failRunWithCheckpoint marks a run as failed_resumable with a checkpoint,
// enabling resume from the last completed node. Falls back to a regular
// (non-resumable) failure if the checkpoint write fails.
func (e *Engine) failRunWithCheckpoint(rs *runState, nodeID, reason string) error {
	return e.withCheckpointFailureWrites(rs, nil, func(writeCtx context.Context) error {
		return e.publishFailureWithCheckpoint(writeCtx, rs, nodeID, reason)
	})
}

// publishFailureWithCheckpoint writes only through the invocation's local
// sequence. The runState still carries the original execution context.
func (e *Engine) publishFailureWithCheckpoint(writeCtx context.Context, rs *runState, nodeID, reason string) error {
	writes := terminationScope(writeCtx)
	e.captureFailureBoundary(rs, nodeID)
	cp := buildCheckpoint(rs, nodeID)
	if storeErr := writes.Do(func(c context.Context) error {
		return e.recordFailureResumable(c, rs.runID, cp, reason, ErrCodeExecutionFailed)
	}); storeErr != nil {
		e.logger.Error("failed to persist resumable failure: %v", storeErr)
		return writes.Do(func(c context.Context) error { return e.failRun(c, rs.runID, nodeID, reason) })
	}
	return writes.Do(func(c context.Context) error {
		return e.emitRunFailedAndReturn(c, rs.runID, nodeID, reason, ErrCodeExecutionFailed)
	})
}

// simpleContextCause recognizes only a context failure under neutral wrappers.
// A classified runtime error or any joined sibling cause keeps its own outcome.
func simpleContextCause(err, target error) bool {
	if IsAmbiguousEffect(err) {
		return false
	}
	for err != nil {
		if err == target {
			return true
		}
		if rt, ok := err.(*RuntimeError); ok && rt.Code != ErrCodeExecutionFailed {
			return false
		}
		single, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = single.Unwrap()
	}
	return false
}

// failRunErrWithCheckpoint is the checkpoint-aware variant of failRunErr.
func (e *Engine) failRunErrWithCheckpoint(rs *runState, nodeID string, origErr error) error {
	if errors.Is(origErr, errCorrectionOwnershipLost) {
		return origErr
	}
	if store.IsValueFormatRefusal(origErr) {
		return origErr
	}
	// Non-model nodes can wrap bare cancellation in EXECUTION_FAILED. The
	// same cancellation handler must persist their terminal state under the
	// original context values, detached only from the completed request.
	if rs.ctx != nil && terminationScope(rs.ctx) == nil && rs.ctx.Err() != nil && simpleContextCause(origErr, rs.ctx.Err()) {
		return e.handleContextDoneWithDetail(rs, nodeID, rs.ctx.Err(), stopDetail(origErr))
	}

	return e.withCheckpointFailureWrites(rs, origErr, func(writeCtx context.Context) error {
		return e.publishFailureErrWithCheckpoint(writeCtx, rs, nodeID, origErr)
	})
}

func (e *Engine) publishFailureErrWithCheckpoint(writeCtx context.Context, rs *runState, nodeID string, origErr error) error {
	writes := terminationScope(writeCtx)
	var rtErr *RuntimeError
	if errors.As(origErr, &rtErr) {
		rtErr.Code = recordedCode(rtErr, origErr)
		// Only on this branch: the fallthrough below goes through
		// failRunWithCheckpoint, which captures for itself.
		e.captureFailureBoundary(rs, nodeID)
		// A stop on a usage window parks the run until the window reopens;
		// the span since the last booking only met the wall, so it leaves
		// the duration budget BEFORE the checkpoint a resume reads its carry
		// from is built.
		var refunded time.Duration
		if rtErr.Code == ErrCodeUsageLimitBlocked {
			refunded = rs.budget.refundSpanSinceBooking()
		}
		cp := buildCheckpoint(rs, nodeID)
		remedy := RemedyOf(rtErr)
		if storeErr := writes.Do(func(c context.Context) error {
			return e.recordFailureResumable(c, rs.runID, cp, remedy.Annotate(rtErr.Message), rtErr.Code)
		}); storeErr != nil {
			e.logger.Error("failed to persist resumable failure: %v", storeErr)
			return writes.Do(func(c context.Context) error { return e.failRunErr(c, rs.runID, nodeID, origErr) })
		}
		// Preserve the original *RuntimeError identity so callers can
		// errors.As back to the same value; the helper would otherwise
		// allocate a fresh one.
		data := map[string]any{
			"error":     rtErr.Message,
			"code":      string(rtErr.Code),
			"resumable": true,
		}
		if refunded > 0 {
			// What the operator reads on the timeline: this attempt's
			// minutes did not count against max_duration.
			data["duration_refunded_ms"] = refunded.Milliseconds()
		}
		remedy.Record(data)
		if err := writes.Do(func(c context.Context) error { return e.emit(c, rs.runID, store.EventRunFailed, nodeID, data) }); err != nil {
			e.logger.Warn("failed to emit run_failed event: %v", err)
		}
		if rtErr.NodeID == "" {
			rtErr.NodeID = nodeID
		}
		return rtErr
	}
	return e.publishFailureWithCheckpoint(writeCtx, rs, nodeID, origErr.Error())
}

// ---------------------------------------------------------------------------
// Context handling
// ---------------------------------------------------------------------------

// handleContextDoneWithCheckpoint handles context cancellation or deadline
// exceeded, preserving the checkpoint so the run can be resumed. rs.ctx is
// already done when we get here, so we detach for the store writes using
// context.WithoutCancel — values (tenant/user identity for cloud mode) are
// preserved but cancellation is not. The write budget defaults to five seconds;
// WithTerminationWriteTimeout overrides this local budget and the two checkpoint-
// failure doors; it does not detach the execution context.
func (e *Engine) handleContextDoneWithCheckpoint(rs *runState, nodeID string, ctxErr error) error {
	return e.handleContextDoneWithDetail(rs, nodeID, ctxErr, "")
}

// handleContextDoneWithDetail is handleContextDoneWithCheckpoint whose
// recorded reason carries detail — what the stopped node's work said
// (stopDetail, read by failRunErrWithCheckpoint, the door a subbot node's
// failure takes) — after the stop's own words; "" records the stop alone.
// The detail passes the run's scrubber first and is then cut at
// subbotDetailLimit (boundedDetail): cutting a scrubbed text cannot expose
// what the scrubber withheld, and an unbounded child stderr cannot bloat
// the parent's record.
func (e *Engine) handleContextDoneWithDetail(rs *runState, nodeID string, ctxErr error, detail string) (result error) {
	detail = boundedDetail(e.recordedFailureText(detail))
	withDetail := func(reason string) string {
		if detail == "" {
			return reason
		}
		return reason + "; " + detail
	}
	defer func() {
		if result != nil {
			result = &terminationSettledError{cause: result, owner: rs}
		}
	}()

	e.captureFailureBoundary(rs, nodeID)
	writes := e.newTerminationWrites(rs.ctx, ctxErr)
	defer writes.Close()
	cp := buildCheckpoint(rs, nodeID)
	if errors.Is(ctxErr, context.Canceled) {
		if errors.Is(context.Cause(rs.ctx), ErrRunInterrupted) {
			reason := withDetail(fmt.Sprintf("interrupted at node %s (resumable)", nodeID))
			if err := writes.Do(func(c context.Context) error {
				return e.recordFailureResumable(c, rs.runID, cp, reason, store.FailureInterrupted)
			}); err != nil {
				e.logger.Error("failed to persist resumable interruption: %v", err)
				return writes.Result(writes.Do(func(c context.Context) error { return e.failRun(c, rs.runID, nodeID, reason) }))
			}
			if err := writes.Do(func(c context.Context) error {
				return e.emit(c, rs.runID, store.EventRunFailed, nodeID, map[string]any{"error": reason, "code": string(store.FailureInterrupted), "resumable": true, "interrupted": true})
			}); err != nil {
				e.logger.Warn("failed to emit run_failed event: %v", err)
			}
			return writes.Result(fmt.Errorf("%w: at node %s", ErrRunInterrupted, nodeID))
		}
		// A missing checkpoint remains best-effort for operator cancellation, but
		// an authority refusal prevents every subsequent callback in this sequence.
		if err := writes.Do(func(c context.Context) error { return e.store.SaveCheckpoint(c, rs.runID, cp) }); err != nil {
			e.logger.Error("failed to save checkpoint on cancellation: %v", err)
		}
		cancellable := []store.RunStatus{store.RunStatusRunning, store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator, store.RunStatusFailedResumable}
		changed := false
		if err := writes.Do(func(c context.Context) error {
			var err error
			changed, err = e.recordFailureStatusIf(c, rs.runID, store.RunStatusCancelled, withDetail("run cancelled"), store.FailureCancelled, cancellable)
			return err
		}); err != nil {
			// A failed status write is not a persisted cancellation.
			return writes.Result(err)
		}
		if !changed {
			// The publisher may already have cancelled this run. Preserve that reason;
			// a different terminal status must not be announced as our cancellation.
			var current *store.Run
			if err := writes.Do(func(c context.Context) error {
				var err error
				_, current, err = store.LoadRunWithValueFormat(c, e.store, rs.runID)
				return err
			}); err != nil {
				return writes.Result(err)
			}
			if current.Status != store.RunStatusCancelled {
				return fmt.Errorf("%w: cancellation found status %s", ErrAdmissionStopped, current.Status)
			}
			e.logger.Debug("run %s already cancelled — keeping its recorded reason", rs.runID)
		}
		if err := writes.Do(func(c context.Context) error {
			return e.emit(c, rs.runID, store.EventRunCancelled, nodeID, map[string]any{"reason": "context cancelled"})
		}); err != nil {
			e.logger.Warn("failed to emit run_cancelled event: %v", err)
		}
		return writes.Result(fmt.Errorf("%w: interrupted at node %s", ErrRunCancelled, nodeID))
	}
	reason := withDetail(fmt.Sprintf("timeout: %s", ctxErr.Error()))
	if err := writes.Do(func(c context.Context) error {
		return e.recordFailureResumable(c, rs.runID, cp, reason, ErrCodeTimeout)
	}); err != nil {
		e.logger.Error("failed to persist resumable failure: %v", err)
		return writes.Result(writes.Do(func(c context.Context) error { return e.failRun(c, rs.runID, nodeID, reason) }))
	}
	if err := writes.Do(func(c context.Context) error {
		return e.emit(c, rs.runID, store.EventRunFailed, nodeID, map[string]any{"error": reason, "code": string(ErrCodeTimeout), "resumable": true})
	}); err != nil {
		e.logger.Warn("failed to emit run_failed event: %v", err)
	}
	return writes.Result(&RuntimeError{Code: ErrCodeTimeout, Message: reason, NodeID: nodeID})
}

// wrapContextErr wraps a context error for branch-level reporting. The
// cancellation keeps the context's error in its chain (runCancelledError),
// so the run-level handler recognises the run's own cancellation in a
// fan-out's result (simpleContextCause) instead of recording it as a node
// failure.
func (e *Engine) wrapContextErr(ctxErr error) error {
	if IsAmbiguousEffect(ctxErr) {
		return ctxErr
	}
	if errors.Is(ctxErr, context.Canceled) {
		return &runCancelledError{ctxErr: ctxErr}
	}
	return ctxErr
}

// runCancelledError is ErrRunCancelled carrying the context error that
// ended the work: errors.Is reports ErrRunCancelled, and the single Unwrap
// reaches the context error itself.
type runCancelledError struct {
	ctxErr error
}

func (e *runCancelledError) Error() string {
	return ErrRunCancelled.Error() + ": " + e.ctxErr.Error()
}

func (e *runCancelledError) Is(target error) bool { return target == ErrRunCancelled }

func (e *runCancelledError) Unwrap() error { return e.ctxErr }

// SubbotStoppedError is a subbot node whose parent context had ended when
// its child's verdict was read (runSubbotNode): the node stopped because
// the parent did — an operator cancel, a drain, or the parent's deadline.
// The parent's context error travels as the single Unwrap, so the parent's
// run-level handler settles its own cancellation, interruption or timeout
// (handleContextDoneWithCheckpoint) instead of a node failure; Detail
// carries the child's words into that record.
type SubbotStoppedError struct {
	NodeID string
	Source string
	// Cause is context.Cause of the parent's context: ErrRunInterrupted for
	// a drain, context.DeadlineExceeded for a deadline, the context error
	// itself for a plain cancel.
	Cause error
	Child error
	// ctxErr is the parent context's Err(), the identity the run-level
	// handler matches.
	ctxErr error
}

func (e *SubbotStoppedError) Error() string {
	return fmt.Sprintf("subbot %q (source %q): stopped with the parent run (%v); %s", e.NodeID, e.Source, e.Cause, e.Detail())
}

// Detail is what the child returned, for the parent's failure record.
func (e *SubbotStoppedError) Detail() string {
	return fmt.Sprintf("subbot %q child returned: %v", e.NodeID, e.Child)
}

func (e *SubbotStoppedError) Unwrap() error { return e.ctxErr }

// subbotDetailLimit bounds, in bytes, the child's words a parent's stop
// record carries; subbotDetailTruncated marks a cut.
const (
	subbotDetailLimit     = 4 << 10
	subbotDetailTruncated = "…(truncated)"
)

// boundedDetail cuts detail at subbotDetailLimit on a rune boundary and
// marks the cut.
func boundedDetail(detail string) string {
	if len(detail) <= subbotDetailLimit {
		return detail
	}
	cut := subbotDetailLimit
	for cut > 0 && !utf8.RuneStart(detail[cut]) {
		cut--
	}
	return detail[:cut] + subbotDetailTruncated
}

// stopDetail is the detail a stopped node's error carries for the run's
// stop record: a subbot's child words, "" otherwise.
func stopDetail(err error) string {
	var stopped *SubbotStoppedError
	if errors.As(err, &stopped) {
		return stopped.Detail()
	}
	return ""
}

// nodeFailureCode preserves a node's classified failure at execution boundaries.
func nodeFailureCode(err error) ErrorCode { return ClassifyError(err) }

func wrapNodeFailure(nodeID, message string, err error) *RuntimeError {
	return &RuntimeError{Code: nodeFailureCode(err), NodeID: nodeID, Message: message, Cause: err}
}
