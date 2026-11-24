package runtime

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/store"
	"time"
)

// AbortPreparedRun is only for a failed preparation before the engine handoff.
// A successful exclusive birth supplies a preimage, never execution authority.
// PreparedStopOptions applies only to this cleanup call. Zero chooses the existing
// setup defaults; negative values remove that bound. No policy is stored on Run.
type PreparedStopOptions struct {
	MaxAttempts int
	Timeout     time.Duration
}

func preparedStopOptions(all []PreparedStopOptions) (PreparedStopOptions, error) {
	if len(all) > 1 {
		return PreparedStopOptions{}, errors.New("prepared stop: multiple policies")
	}
	var o PreparedStopOptions
	if len(all) == 1 {
		o = all[0]
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 3
	}
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Second
	}
	return o, nil
}
func AbortPreparedRun(ctx context.Context, runs store.RunStore, p store.PreparedRunInvocation, cause error, options ...PreparedStopOptions) error {
	if !preparedCancellation(ctx, cause) {
		return cause
	}
	return retirePreparedRun(ctx, runs, p, cause, options...)
}

// RetirePreparedRun closes a child birth abandoned during host setup, before
// handing it to Engine.RunPrepared. Unlike AbortPreparedRun, ordinary setup
// errors also retire the birth. It never grants authority over an existing run
// or a successor: the original format, epoch and startable status still gate
// the outcome CAS. Callers must not defer this across engine execution.
func RetirePreparedRun(ctx context.Context, runs store.RunStore, p store.PreparedRunInvocation, cause error, options ...PreparedStopOptions) error {
	if cause == nil || ctx == nil || errors.Is(cause, ErrAdmissionStopped) || errors.Is(cause, ErrResumeSuperseded) || errors.Is(cause, errCorrectionOwnershipLost) || store.IsValueFormatRefusal(cause) {
		return cause
	}
	return retirePreparedRun(ctx, runs, p, cause, options...)
}

func retirePreparedRun(ctx context.Context, runs store.RunStore, p store.PreparedRunInvocation, cause error, options ...PreparedStopOptions) error {
	binding, original, ok := p.OriginalBirth()
	if !ok {
		return cause
	}
	// Compare the immutable declaration with this caller's retained context.
	detached := context.WithoutCancel(ctx)
	if err := store.RequireRunValueFormat(detached, binding.RunID); err != nil {
		return err
	}
	f, err := store.ValueFormatForWrite(detached, binding.RunID)
	if err != nil {
		return err
	}
	if f != binding.Format {
		return fmt.Errorf("%w: prepared original differs", store.ErrValueFormatChanged)
	}
	opts, err := preparedStopOptions(options)
	if err != nil {
		return err
	}
	return stopPreparedSnapshot(ctx, runs, binding.RunID, store.RunOutcomeExpectation{Version: original.Version, Epoch: original.Epoch}, original.Status, cause, opts)
}

// PreparedCancellation reports whether AbortPreparedRun retires a prepared
// birth for cause: a failure the cancellation of ctx caused. A launch ends its
// birth any other way only for a failure this reports false.
func PreparedCancellation(ctx context.Context, cause error) bool {
	return preparedCancellation(ctx, cause)
}

func preparedCancellation(ctx context.Context, cause error) bool {
	if cause == nil || ctx == nil || ctx.Err() == nil || errors.Is(cause, ErrAdmissionStopped) || errors.Is(cause, ErrResumeSuperseded) || store.IsValueFormatRefusal(cause) {
		return false
	}
	// A reserved marker selects its classifier exclusively: falling through to
	// the legacy single-Unwrap rule would bypass typed/opaque cause vetoes.
	if store.IsRunBirthPublicationCancellation(cause, ctx.Err()) {
		return reservedPublicationCancellation(cause, ctx.Err())
	}
	return simpleContextCause(cause, ctx.Err())
}

// stopPreparedSnapshot uses the existing outcome writer with extra predicates.
// Version may follow metadata; the original epoch is never refreshed.
func stopPreparedSnapshot(ctx context.Context, runs store.RunStore, id string, original store.RunOutcomeExpectation, initialStatus store.RunStatus, cause error, opts PreparedStopOptions) error {
	if !initialStatus.CanStartExecution() {
		return fmt.Errorf("%w: birth was not startable", ErrAdmissionStopped)
	}
	cap := store.AsRunOutcomeCASStore(runs)
	if cap == nil {
		return errors.Join(cause, errors.New("prepared retirement: store lacks outcome CAS"))
	}
	stopCtx := context.WithoutCancel(ctx)
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		stopCtx, cancel = context.WithTimeout(stopCtx, opts.Timeout)
		defer cancel()
	}
	// A concurrent cancellation does not replace an independent setup error.
	// AbortPreparedRun already admits only context-caused errors; child setup
	// retirement also accepts ordinary business failures.
	classificationCtx := ctx
	if !preparedCancellation(ctx, cause) {
		classificationCtx = context.WithoutCancel(ctx)
	}
	status, msg, code := setupFailureStatus(classificationCtx, "pre-engine preparation", cause)
	// An ambiguous publication/setup effect outranks an outer generic code or
	// a resumable sibling: retirement must not authorize an automatic re-drive.
	if IsAmbiguousEffect(cause) {
		status, code = store.RunStatusFailed, store.FailureAmbiguousEffect
	}

	check := func() (*store.Run, error) {
		_, fresh, err := store.LoadRunWithValueFormat(stopCtx, runs, id)
		if err != nil {
			return nil, err
		}
		if fresh.ExecutionEpoch != original.Epoch || fresh.CASVersion < original.Version || fresh.Status != initialStatus {
			return nil, fmt.Errorf("%w: original birth or acquired entry superseded", ErrAdmissionStopped)
		}
		return fresh, nil
	}
	for attempt := 0; opts.MaxAttempts < 0 || attempt < opts.MaxAttempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Millisecond)
			select {
			case <-stopCtx.Done():
				timer.Stop()
				return stopCtx.Err()
			case <-timer.C:
			}
		}
		fresh, err := check()
		if err != nil {
			return err
		}
		expected := store.RunOutcomeExpectation{Version: fresh.CASVersion, Epoch: original.Epoch}
		changed, err := cap.UpdateRunOutcomeIf(stopCtx, id, expected, status, msg, store.RunOutcomeMeta{Code: code}, []store.RunStatus{initialStatus})
		if err != nil {
			return err
		}
		if changed {
			return setupErrFrom(status, code, cause)
		}
	}
	// A metadata conflict is distinct from an epoch or status takeover.
	if _, err := check(); err != nil {
		return err
	}
	return fmt.Errorf("%w: prepared retirement metadata conflicted", store.ErrRunConflict)
}

// RunPrepared retains the historical Run path for existing invocations. The
// birth stop responsibility ends at admission; no deferred abort spans nodes.
func (e *Engine) RunPrepared(ctx context.Context, id string, inputs map[string]any, p store.PreparedRunInvocation, options ...PreparedStopOptions) error {
	if p.Context == nil {
		return fmt.Errorf("%w: missing prepared context", store.ErrValueFormatCapability)
	}
	if err := store.RequireRunValueFormat(context.WithoutCancel(p.Context), id); err != nil {
		return err
	}
	if err := store.RequireRunValueFormat(context.WithoutCancel(ctx), id); err != nil {
		return err
	}
	original, err := store.ValueFormatForWrite(context.WithoutCancel(p.Context), id)
	if err != nil {
		return err
	}
	current, err := store.ValueFormatForWrite(context.WithoutCancel(ctx), id)
	if err != nil {
		return err
	}
	if original != current {
		return fmt.Errorf("%w: prepared context differs", store.ErrValueFormatChanged)
	}
	if _, _, ok := p.OriginalBirth(); !ok {
		if p.OwnedBirth {
			return fmt.Errorf("%w: missing exclusive birth snapshot", store.ErrValueFormatCapability)
		}
		return e.Run(ctx, id, inputs)
	}
	binding, _, _ := p.OriginalBirth()
	if binding.RunID != id {
		return fmt.Errorf("%w: prepared run ID differs", store.ErrValueFormatChanged)
	}
	if err := store.RequireRunValueFormat(context.WithoutCancel(ctx), id); err != nil {
		return err
	}
	f, err := store.ValueFormatForWrite(context.WithoutCancel(ctx), id)
	if err != nil {
		return err
	}
	if f != binding.Format {
		return fmt.Errorf("%w: run entry differs from original birth", store.ErrValueFormatChanged)
	}
	opts, err := preparedStopOptions(options)
	if err != nil {
		return err
	}
	return e.runPreparedEntry(ctx, id, inputs, &p, opts)
}
func (e *Engine) stopPreparedEntry(ctx context.Context, id string, p *store.PreparedRunInvocation, cause error, opts PreparedStopOptions) error {
	if p == nil || !preparedCancellation(ctx, cause) {
		return cause
	}
	if x := correctionExecutionFromContext(ctx); x != nil {
		if x.authority.RunID != id {
			return errCorrectionOwnershipLost
		}
		// This path uses only this Engine entry's successful acquisition response.
		return stopPreparedSnapshot(ctx, e.store, id, store.RunOutcomeExpectation{Epoch: x.authority.Epoch}, store.RunStatusRunning, cause, opts)
	}
	if start, _ := ctx.Value(preparedQueuedStartKey{}).(*preparedQueuedStart); start != nil && start.grant != nil {
		if start.grant.RunID != id {
			return errCorrectionOwnershipLost
		}
		return stopPreparedSnapshot(ctx, e.store, id, store.RunOutcomeExpectation{Epoch: start.grant.Epoch}, store.RunStatusRunning, cause, opts)
	}
	return AbortPreparedRun(ctx, e.store, *p, cause, opts)
}

// A publication marker establishes only that its leaves are cancellation.
// Typed runtime wrappers and ambiguous effects still own their classification,
// even when another sibling or an outer EXECUTION_FAILED wraps them.
func reservedPublicationCancellation(cause, target error) bool {
	if !store.IsRunBirthPublicationCancellation(cause, target) || IsAmbiguousEffect(cause) {
		return false
	}
	return !errtrack.AnyCause(cause, func(e error) bool {
		var rt *RuntimeError
		return errors.As(e, &rt) && rt.Code != ErrCodeExecutionFailed
	})
}
