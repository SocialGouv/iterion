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
// Arming and claiming are compare-and-swap by design. Arming conditions on the
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
	//
	// `wall` is the instant the wait being armed ends (RunRetryState.Wall):
	// the provider's own word, or the guess a refusal that named none fell
	// back to. It is NOT `at`, which also carries the jitter, the floor, the
	// max_wait ceiling and the shared circuit's deferral. The write clears
	// the probe BUDGET iff it MOVES the wall — the comparison belongs here,
	// in the one update that can make it atomically: a caller comparing the
	// wall it read would race the sweeper's pull. The PACE
	// (RunRetryState.LastProbeAt) is the row's and no arming clears it.
	ScheduleRunRetry(ctx context.Context, runID string, at, wall time.Time, reason, code string, maxAttempts int) (scheduled bool, attempt int, err error)
	// DelayRunRetry moves an already-armed retry without consuming another
	// attempt. expectedAfter is a CAS guard against an operator or another
	// sweeper changing the intent concurrently.
	DelayRunRetry(ctx context.Context, runID string, expectedAfter, delayedUntil time.Time) (delayed bool, err error)
	// PullRunRetry pulls an armed retry FORWARD to probeAt (the sweeper's
	// "now") so the runner's usage preflight — the only authoritative
	// reading of the provider's window — gets to answer before the
	// announced instant. expectedAfter is the same CAS guard DelayRunRetry
	// uses. It spends no attempt and leaves ScheduledAt alone (the max_wait
	// anchor is not a probe's to move); it keeps expectedAfter in
	// RunRetryState.ProbeFrom, counts the probe and refuses a row that
	// already carries a marker.
	PullRunRetry(ctx context.Context, runID string, expectedAfter, probeAt time.Time) (pulled bool, err error)
	// RearmProbedRunRetry re-arms a PROBE-MARKED retry at `at` without
	// spending an attempt or consulting the attempt budget, and consumes
	// the marker. It is how both halves settle a probe that found the
	// window still shut: the runner's preflight re-failure arms the
	// provider's fresh instant, and the sweeper puts the held-aside instant
	// back when a probed resume never reached the runner at all. Reports
	// rearmed=false (no error) when the row carries no marker or is no longer
	// failed_resumable — the caller then takes its ordinary, paid path.
	//
	// `wall` follows ScheduleRunRetry's rule, including the budget: the zero
	// time means "this re-arm brought no news of its own, keep the wall the
	// row is on" — which is what the sweeper's stand-down and every restore
	// of the held-aside instant pass. A probed refusal that named a DIFFERENT
	// instant passes it, moves the wall, and the budget starts over, because
	// that is a different wait.
	RearmProbedRunRetry(ctx context.Context, runID string, at, wall time.Time) (rearmed bool, err error)
	// ClaimRunRetry LEASES an armed retry, conditioning on expectedAfter
	// matching what the caller read and on no live lease existing. The
	// winner sees won=true; every loser sees won=false. The retry stays
	// armed on purpose: a caller that dies before acting would otherwise
	// strand the run with nothing left to re-list it.
	ClaimRunRetry(ctx context.Context, runID string, expectedAfter time.Time) (won bool, err error)
	// ClearRunRetry disarms a retry that has been acted on (the successful
	// resume's counterpart to ClaimRunRetry's lease).
	ClearRunRetry(ctx context.Context, runID string) error
	// AbandonRunRetry records why a run will not be retried again and
	// leaves the retry disarmed. Used when the cause is permanent
	// (admission denied, source no longer resolvable) so the run says why
	// it stopped instead of going quiet.
	//
	// expectedAfter is the armed instant the caller READ, and the write is
	// conditioned on it: settling is irreversible (the continuation goes
	// final and the probe marker holding the provider's word is dropped), so
	// a sweep tick that pulled or re-armed the row in between must not be
	// overwritten. abandoned=false with no error is that lost race — the row
	// belongs to whoever moved it, and the caller reports rather than
	// retries.
	//
	// The zero time is the deliberate escape hatch, for a cause that is
	// about the run's own future rather than this wait: a pull request
	// closed or re-queued under it leaves nothing to wake, whatever instant
	// the row holds.
	AbandonRunRetry(ctx context.Context, runID, reason string, expectedAfter time.Time) (abandoned bool, err error)
}

// AsRunRetryStore returns s as RunRetryStore when the backend can host
// durable retry state, or nil otherwise. Callers MUST nil-check
// (filesystem / local stores return nil).
func AsRunRetryStore(s RunStore) RunRetryStore {
	return capability[RunRetryStore](s)
}
