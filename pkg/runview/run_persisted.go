package runview

import "errors"

// RunPersistedError marks an error returned AFTER a run document exists in the
// store — or after work was handed to something that may already be executing
// it.
//
// It exists because a caller cannot reconstruct the fact from the error.
// `spawnRun` persists the run document and can still fail afterwards on the
// budget-override write — the orphan reconciler then moves that run to
// `failed_resumable` and it resumes — so an error out of Launch does not mean
// no run started. The metering surfaces first answered that by keeping the
// quota slot on EVERY error, which is safe against the under-count but leaves
// the ticket's headline symptom alive: a repeatable launch failure charges one
// monthly unit per attempt. Neither side can be decided from the error's text,
// so the fact travels WITH the result instead: the code that knows writes it,
// and every metering surface reads it with errors.As.
//
// Three sibling surfaces (the trigger spine, the board dispatcher, the retry
// sweeper) used to infer it, and infer it wrong — they refunded on any error,
// on the strength of a comment saying "every error out of Launch means no run
// started". They now read the same fact through RunMayHaveStarted (#1725).
//
// Absence is the claim "this call started nothing", so a new error return that
// follows the creation of a run, or a publish that may have landed, has to
// wrap — that is the one rule this type asks of the code around it. Note the
// claim is about THIS CALL, not about the store: a resume whose run document
// existed all along has started nothing until its message is published.
type RunPersistedError struct {
	// RunID is the run whose document exists, so a caller can say which.
	RunID string
	Err   error
}

func (e *RunPersistedError) Error() string { return e.Err.Error() }
func (e *RunPersistedError) Unwrap() error { return e.Err }

// RunMayHaveStarted reports whether an error out of Launch or Resume leaves a
// run behind — a persisted document, or a queue message the runner may already
// have claimed. A metering surface releases its slot only when this is false.
func RunMayHaveStarted(err error) bool {
	if err == nil {
		return false
	}
	var persisted *RunPersistedError
	if errors.As(err, &persisted) {
		return true
	}
	// A publish that reports failure after the message landed: cloudpublisher's
	// own rollback exists for exactly that case, and says so — "the runner then
	// claims and executes the revision this call published".
	var queue *QueueUnavailableError
	return errors.As(err, &queue)
}
