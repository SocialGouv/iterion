package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// WithTerminationWriteTimeout controls bookkeeping in the context-stop handler
// and the two checkpoint-failure doors. Nested failure publication borrows its
// existing sequence and remaining budget.
// Zero keeps the historical five-second budget; a negative value is
// unbounded. This does not change preparation, branch grace or spend booking.
func WithTerminationWriteTimeout(d time.Duration) EngineOption {
	return func(e *Engine) { e.terminationWriteTimeout = d }
}

type terminationWrites struct {
	ctx      context.Context
	close    context.CancelFunc
	original error
	refusal  error
}

func terminationRefusal(err error) bool {
	return errors.Is(err, errCorrectionOwnershipLost) || store.IsValueFormatRefusal(err)
}

func (e *Engine) newTerminationWrites(ctx context.Context, original error) *terminationWrites {
	budget := e.terminationWriteTimeout
	if budget == 0 {
		budget = 5 * time.Second
	}
	writeCtx := context.WithoutCancel(ctx)
	close := func() {}
	if budget > 0 {
		writeCtx, close = context.WithTimeout(writeCtx, budget)
	}
	w := &terminationWrites{close: close, original: original}
	w.ctx = context.WithValue(writeCtx, terminationScopeKey{}, w)
	if terminationRefusal(original) {
		w.refusal = original
	}
	return w
}

// Do retains the first authority refusal and never calls a later operation
// after it. Ordinary storage errors remain for the caller's existing policy.
func (w *terminationWrites) Do(write func(context.Context) error) error {
	if w.refusal != nil {
		return w.refusal
	}
	err := write(w.ctx)
	w.observe(err)
	if w.refusal != nil {
		return w.refusal
	}
	return err
}
func (w *terminationWrites) Close() { w.close() }

// Result cannot announce cancellation/interruption after a refused write. An
// independently established business or ambiguous verdict survives beside the
// refusal, so neither its classification nor the authority failure is erased.
func (w *terminationWrites) Result(result error) error {
	if w.refusal == nil {
		return result
	}
	established := IsAmbiguousEffect(w.original) || errors.Is(w.original, ErrDeliberateFailure) || independentTerminationVerdict(w.original)
	if established && !errors.Is(w.refusal, w.original) {
		return errors.Join(w.original, w.refusal)
	}
	return w.refusal
}

// This scope exists only during the synchronous termination sequence. It carries
// no durable ownership. Only the context-stop handler and checkpoint-failure
// doors open a sequence; their ordinary failure-writer fallbacks borrow it.
type terminationScopeKey struct{}

func (w *terminationWrites) observe(err error) {
	if w.refusal == nil && terminationRefusal(err) {
		w.refusal = err
	}
}
func terminationScope(ctx context.Context) *terminationWrites {
	w, _ := ctx.Value(terminationScopeKey{}).(*terminationWrites)
	return w
}

// A handled stop may itself return a context error from its required I/O.
// That error is still discoverable, but it must not enter a second stop writer.
type terminationSettledError struct {
	cause error
	owner *runState
}

func (e *terminationSettledError) Error() string { return e.cause.Error() }
func (e *terminationSettledError) Unwrap() error { return e.cause }

func independentTerminationVerdict(err error) bool {
	if err == nil {
		return false
	}
	var rt *RuntimeError
	if errors.As(err, &rt) && rt != nil && rt.Code != ErrCodeExecutionFailed {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			if independentTerminationVerdict(child) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return independentTerminationVerdict(wrapped.Unwrap())
	case interface{ As(any) bool }:
		// Match the existing ambiguity reader: redaction hides Unwrap but preserves As.
		var many interface{ Unwrap() []error }
		if wrapped.As(&many) && many != nil {
			for _, child := range many.Unwrap() {
				if independentTerminationVerdict(child) {
					return true
				}
			}
		}
		var one interface{ Unwrap() error }
		if wrapped.As(&one) && one != nil {
			return independentTerminationVerdict(one.Unwrap())
		}
	}
	return false
}

// withCheckpointFailureWrites separates the original execution context from
// failure publication. A nested call borrows the caller's sequence: it neither
// renews the timeout nor closes that caller's context.
func (e *Engine) withCheckpointFailureWrites(rs *runState, original error, publish func(context.Context) error) (result error) {
	if terminationRefusal(original) {
		return original
	}
	writes := terminationScope(rs.ctx)
	if writes == nil {
		writes = e.newTerminationWrites(rs.ctx, original)
		defer writes.Close()
	}
	defer func() {
		if result != nil {
			result = &terminationSettledError{cause: result, owner: rs}
		}
	}()
	result = writes.Result(writes.Do(publish))
	// The borrowed sequence may have a different original cause. Preserve a
	// newly established verdict without replacing the sequence's own cause.
	if writes.refusal != nil && (IsAmbiguousEffect(original) || errors.Is(original, ErrDeliberateFailure) || independentTerminationVerdict(original)) && !errors.Is(result, original) {
		result = errors.Join(original, result)
	}
	return result
}
