package store

import (
	"context"
	"time"
)

// RunRetryStore is an optional interface a store implements to arm and
// claim automatic retries for runs that failed on a provider quota window.
//
// It is deliberately a CAPABILITY rather than part of RunStore: the wait
// can be days long, so it needs a durable row a periodic sweeper can scan,
// which only the cloud (Mongo) store has. The local/filesystem runtime
// never crosses a queue and retries in-process instead, so there is
// nothing to sweep there. Callers MUST nil-check via AsRunRetryStore.
//
// Both methods are compare-and-swap by design. Arming conditions on the
// run still being failed_resumable and under its attempt budget, so a run
// an operator resumed by hand in the meantime is not re-armed behind their
// back. Claiming conditions on the exact retry_after value it read, which
// is what makes "exactly one replica resumes this run" true without a
// leader election — the same shape cloudsched's tick claim uses.
type RunRetryStore interface {
	// ScheduleRunRetry arms a retry at `at`, iff the run is still
	// failed_resumable and has attempts left. Returns scheduled=false
	// (with no error) when either precondition fails — a lost race and an
	// exhausted budget are ordinary outcomes, not failures. The returned
	// attempt is the 1-based number of the retry just armed.
	ScheduleRunRetry(ctx context.Context, runID string, at time.Time, reason, code string, maxAttempts int) (scheduled bool, attempt int, err error)
	// ClaimRunRetry takes ownership of an armed retry, conditioning on
	// expectedAfter matching what the caller read. The winner sees
	// won=true and the retry disarmed; every loser sees won=false.
	ClaimRunRetry(ctx context.Context, runID string, expectedAfter time.Time) (won bool, err error)
	// AbandonRunRetry records why a run will not be retried again and
	// leaves the retry disarmed. Used when the cause is permanent
	// (admission denied, source no longer resolvable) so the run says why
	// it stopped instead of going quiet.
	AbandonRunRetry(ctx context.Context, runID, reason string) error
}

// AsRunRetryStore returns s as RunRetryStore when the backend can host
// durable retry state, or nil otherwise. Callers MUST nil-check
// (filesystem / local stores return nil).
func AsRunRetryStore(s RunStore) RunRetryStore {
	if s == nil {
		return nil
	}
	r, _ := s.(RunRetryStore)
	return r
}
