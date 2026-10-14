// Package retryprobetest models the durable retry row the usage-window
// pull-forward probe acts on, so the real decision code of pkg/runner and
// pkg/server can be driven over long random event sequences.
//
// Why a model and not the Mongo store: the invariants the probe has to keep
// are about the INTERLEAVING of a row's states (who pays for an arming, which
// instant survives it, what is refunded, what is counted), and a sequence of
// forty steps has to run in milliseconds and on a machine with no database.
// Why a model and not the existing per-test doubles: those record calls, which
// cannot answer "what does the row look like after step 23".
//
// The faithfulness rule here is absolute, because a model kinder than the
// store proves nothing: every method below mirrors ONE Mongo update document
// of pkg/store/mongo/runs_retry.go, filter term by filter term, including the
// millisecond truncation BSON applies to every instant (a CAS that compares
// nanoseconds would match where Mongo's would not). The pairing is pinned
// from the other side by the ITERION_TEST_MONGO_URI-gated conformance test
// (pkg/store/mongo/runs_retry_model_conformance_test.go), which replays the
// same generated op sequences against a real replica set and this model and
// compares the row after every step.
package retryprobetest

import (
	"context"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ClaimLease mirrors the unexported retryClaimLease of pkg/store/mongo: how
// long a claimed-but-not-resumed retry stays off-limits.
const ClaimLease = 10 * time.Minute

// Store is the in-memory twin of the Mongo run document's retry_state.
//
// It carries one run, which is what the probe's invariants are about. The
// error fields inject the store blips the two settlement paths must survive;
// the counters are the observations a driver reads between steps.
type Store struct {
	// Embedded nil interface: an unimplemented RunStore method panics
	// loudly rather than passing silently, the discipline the retry fakes
	// next door already use.
	store.RunStore

	mu  sync.Mutex
	run *store.Run

	// PullErr/RearmErr/ScheduleErr/AbandonErr make the next call of that
	// method fail. Cleared on use when *Once is set, so a driver can model
	// a TRANSIENT blip — a fake that fails forever cannot tell a path that
	// gives up from one that tries again.
	PullErr, RearmErr, ScheduleErr, AbandonErr error
	PullErrOnce, RearmErrOnce                  bool

	// Observations, all monotonic.
	Pulls           int
	FreeRearms      int
	PaidArmings     int
	RefusedArmings  int
	Claims          int
	Clears          int
	Delays          int
	CircuitFailures int
	Abandons        []string
	Events          []store.Event
}

// New builds a store holding run as its only document.
func New(run *store.Run) *Store { return &Store{run: run} }

// bsonTime is the instant as the database would hold it: UTC, truncated to
// the millisecond BSON stores. Every CAS below compares truncated values on
// both sides, which is what makes a model CAS answer what Mongo's answers.
func bsonTime(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

func bsonPtr(t time.Time) *time.Time { v := bsonTime(t); return &v }

// sameInstant is the CAS comparison, with BOTH sides as the database holds
// them: a caller passing an instant it read back has already been truncated,
// and a row seeded by hand must not be comparable where Mongo's would not be.
func sameInstant(a *time.Time, b time.Time) bool {
	return a != nil && bsonTime(*a).Equal(bsonTime(b))
}

// Row is a deep copy of the retry state, for a caller that wants to look at
// the row without holding the document the next step mutates.
func (s *Store) Row() *store.RunRetryState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyRetryState(s.run)
}

// Doc is a deep copy of the whole run document.
func (s *Store) Doc() *store.Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyRun(s.run)
}

// Mutate hands the live document to fn under the store's lock, for the steps
// that model something OUTSIDE the retry row — an operator cancelling the
// run, the passage of time that ends a provider wall, a credential's
// reopening being recorded.
func (s *Store) Mutate(fn func(*store.Run)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.run)
}

func copyRetryState(r *store.Run) *store.RunRetryState {
	if r == nil || r.RetryState == nil {
		return nil
	}
	out := *r.RetryState
	out.RetryAfter = copyInstant(r.RetryState.RetryAfter)
	out.ScheduledAt = copyInstant(r.RetryState.ScheduledAt)
	out.ClaimedAt = copyInstant(r.RetryState.ClaimedAt)
	out.ProbeFrom = copyInstant(r.RetryState.ProbeFrom)
	out.LastProbeAt = copyInstant(r.RetryState.LastProbeAt)
	return &out
}

func copyRun(r *store.Run) *store.Run {
	if r == nil {
		return nil
	}
	out := *r
	out.RetryState = copyRetryState(r)
	out.SkippedCredReopensAt = copyInstant(r.SkippedCredReopensAt)
	return &out
}

func copyInstant(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// state returns the live retry state, creating it the way a Mongo $set on a
// dotted retry_state.* path creates the embedded document.
//
// Called only where the update is about to LAND: a refused update writes
// nothing at all, so a row that never armed must still carry no retry_state
// — a model that materialised an all-zero one would answer "attempts=0"
// where the store answers "absent", and the type-bracketing trap that costs
// the first arming of every run lives exactly in that difference.
func (s *Store) state() *store.RunRetryState {
	if s.run.RetryState == nil {
		s.run.RetryState = &store.RunRetryState{}
	}
	return s.run.RetryState
}

// attempts is the row's attempt count, 0 when there is no retry state — the
// absent case the filters have to answer.
func (s *Store) attempts() int {
	if s.run.RetryState == nil {
		return 0
	}
	return s.run.RetryState.Attempts
}

// LoadRun answers with a COPY, like a driver decoding a fresh document: a
// reader that kept the store's own pointer would see writes it never read.
func (s *Store) LoadRun(_ context.Context, _ string) (*store.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyRun(s.run), nil
}

// AppendEvent keeps the run's timeline: the reset_source of each arming is
// what an operator reads to tell a free re-arm from a paid one.
func (s *Store) AppendEvent(_ context.Context, _ string, ev store.Event) (*store.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Events = append(s.Events, ev)
	return &ev, nil
}

// ScheduleRunRetry mirrors mongo.Store.ScheduleRunRetry: the attempt budget
// and the status are in the FILTER, the paid arming resets the probe ledger,
// and attempts is incremented by the same write.
func (s *Store) ScheduleRunRetry(_ context.Context, _ string, at time.Time, reason, code string, maxAttempts int) (bool, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ScheduleErr != nil {
		return false, 0, s.ScheduleErr
	}
	if maxAttempts <= 0 {
		return false, 0, nil
	}
	// filter: status == failed_resumable, attempts absent or < maxAttempts.
	if s.run.Status != store.RunStatusFailedResumable || s.attempts() >= maxAttempts {
		s.RefusedArmings++
		return false, 0, nil
	}
	st := s.state()
	now := bsonTime(time.Now())
	st.RetryAfter = bsonPtr(at)
	st.ScheduledAt = &now
	st.Reason, st.Code = reason, code
	st.LastError = ""
	st.ClaimedAt = nil
	// A PAID arming is a new wait: the probe ledger starts over — including
	// the arming that merely restored the announced instant, which arms the
	// same wait (store.ProbeMarkerTTL states what that costs).
	st.ProbeFrom, st.ProbeCount, st.LastProbeAt = nil, 0, nil
	st.Attempts++
	s.run.ContinuationState = store.ContinuationRetryArmed
	s.PaidArmings++
	return true, st.Attempts, nil
}

// DelayRunRetry mirrors mongo.Store.DelayRunRetry: an exact retry_after CAS,
// no attempt, and scheduled_at preserved (or seeded from the CAS value) so a
// repeatedly deferred circuit cannot postpone one run forever.
func (s *Store) DelayRunRetry(_ context.Context, _ string, expectedAfter, delayedUntil time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.run.RetryState
	if st == nil || s.run.Status != store.RunStatusFailedResumable || !sameInstant(st.RetryAfter, expectedAfter) {
		return false, nil
	}
	st.RetryAfter = bsonPtr(delayedUntil)
	if st.ScheduledAt == nil {
		st.ScheduledAt = bsonPtr(expectedAfter)
	}
	st.ClaimedAt = nil
	s.Delays++
	return true, nil
}

// PullRunRetry mirrors mongo.Store.PullRunRetry: the status and the exact
// retry_after are the CAS, the ABSENCE of probe_from is the third filter term
// (a second pull over a live marker would overwrite the provider's instant),
// no attempt is charged and scheduled_at is left alone.
func (s *Store) PullRunRetry(_ context.Context, _ string, expectedAfter, probeAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PullErr != nil {
		err := s.PullErr
		if s.PullErrOnce {
			s.PullErr = nil
		}
		return false, err
	}
	st := s.run.RetryState
	if st == nil || s.run.Status != store.RunStatusFailedResumable ||
		!sameInstant(st.RetryAfter, expectedAfter) ||
		st.ProbeFrom != nil {
		return false, nil
	}
	now := bsonTime(time.Now())
	st.RetryAfter = bsonPtr(probeAt)
	st.ProbeFrom = bsonPtr(expectedAfter)
	st.LastProbeAt = &now
	st.ProbeCount++
	s.Pulls++
	return true, nil
}

// RearmProbedRunRetry mirrors mongo.Store.RearmProbedRunRetry: the marker's
// PRESENCE and the status are the filter, attempts is untouched (neither the
// counter nor a budget check), probe_count and last_probe_at survive — a
// ledger a probe's own re-arming could reset would bound nothing — and the
// marker is consumed.
func (s *Store) RearmProbedRunRetry(_ context.Context, _ string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.RearmErr != nil {
		err := s.RearmErr
		if s.RearmErrOnce {
			s.RearmErr = nil
		}
		return false, err
	}
	st := s.run.RetryState
	if st == nil || s.run.Status != store.RunStatusFailedResumable || st.ProbeFrom == nil {
		return false, nil
	}
	st.RetryAfter = bsonPtr(at)
	st.ProbeFrom = nil
	st.ClaimedAt = nil
	s.run.ContinuationState = store.ContinuationRetryArmed
	s.FreeRearms++
	return true, nil
}

// ClaimRunRetry mirrors mongo.Store.ClaimRunRetry: a LEASE, not a disarm —
// retry_after deliberately stays, so a pod that dies before resuming leaves
// the row re-claimable one lease later.
func (s *Store) ClaimRunRetry(_ context.Context, _ string, expectedAfter time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.run.RetryState
	now := bsonTime(time.Now())
	if st == nil || s.run.Status != store.RunStatusFailedResumable || !sameInstant(st.RetryAfter, expectedAfter) {
		return false, nil
	}
	if st.ClaimedAt != nil && !st.ClaimedAt.Before(now.Add(-ClaimLease)) {
		return false, nil
	}
	st.ClaimedAt = &now
	s.Claims++
	return true, nil
}

// ClearRunRetry mirrors mongo.Store.ClearRunRetry: retry_after and the lease
// go, the probe ledger deliberately SURVIVES — every resume clears the retry,
// including the one a probe just pulled forward, and it runs before the pod
// reaches the usage preflight the marker exists for.
func (s *Store) ClearRunRetry(_ context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// $unset on a path whose parent document is absent creates nothing.
	if st := s.run.RetryState; st != nil {
		st.RetryAfter, st.ClaimedAt = nil, nil
	}
	s.Clears++
	return nil
}

// AbandonRunRetry mirrors mongo.Store.AbandonRunRetry: not conditioned on
// status, records the reason, drops retry_after AND the marker, and declares
// the continuation final — which is what makes settling a probed row
// irreversible.
func (s *Store) AbandonRunRetry(_ context.Context, _, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AbandonErr != nil {
		return s.AbandonErr
	}
	// $set on retry_state.last_error creates the embedded document when it
	// is absent, so this is the one operation here that materialises it.
	st := s.state()
	st.LastError = reason
	st.RetryAfter, st.ProbeFrom = nil, nil
	s.run.ContinuationState = store.ContinuationFinal
	s.Abandons = append(s.Abandons, reason)
	return nil
}

// RecordRetryFailure is the shared circuit's write. The state stays nil (no
// breaker opens in these sequences); what the drivers read is the COUNT, the
// thing a probe's self-inflicted refusal must never feed.
func (s *Store) RecordRetryFailure(_ context.Context, _, _ string, _ time.Time, _ int, _ time.Duration) (*store.RetryCircuitState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CircuitFailures++
	return nil, nil
}

func (s *Store) RetryCircuitOpen(context.Context, string, time.Time) (*store.RetryCircuitState, error) {
	return nil, nil
}

func (s *Store) RecordRetrySuccess(context.Context, string, time.Time) error { return nil }

// DueRows mirrors the filter of mongo.Store.ListRunsDueForRetry: armed,
// failed_resumable, retry_after <= before. Answers deep copies, because
// resumeDueRetry's refund and anchor read the instant the SCAN saw, not the
// row as it stands when it concludes.
func (s *Store) DueRows(before time.Time, limit int) []*store.RunRetryState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.run.RetryState
	if s.run.Status != store.RunStatusFailedResumable || st == nil || st.RetryAfter == nil {
		return nil
	}
	if st.RetryAfter.After(bsonTime(before)) {
		return nil
	}
	return takeScanLimit([]*store.RunRetryState{copyRetryState(s.run)}, limit)
}

// takeScanLimit applies the limit the way both Mongo scans do: a
// non-positive or absurd one is COERCED to 100 before Find sees it, so "no
// limit given" lists rows in the store and must list them here too — a model
// that answered nil on limit 0 would stand in for a scan that returns rows.
func takeScanLimit(rows []*store.RunRetryState, limit int) []*store.RunRetryState {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// ProbeRows mirrors the filter of mongo.Store.ListRunsForRetryProbe: a
// usage-window wait that is NOT yet due (more than floor away), carries no
// marker, is under the probe budget, was last probed at least a floor ago and
// is not under a live lease. A non-positive budget means probing is off.
func (s *Store) ProbeRows(now time.Time, floor time.Duration, maxProbes, limit int) []*store.RunRetryState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxProbes <= 0 {
		return nil
	}
	at := bsonTime(now)
	st := s.run.RetryState
	switch {
	case s.run.Status != store.RunStatusFailedResumable, st == nil, st.RetryAfter == nil:
		return nil
	case st.Reason != "usage_window":
		return nil
	case !st.RetryAfter.After(at.Add(floor)):
		return nil
	case st.ProbeFrom != nil:
		return nil
	case st.ProbeCount >= maxProbes:
		return nil
	case st.LastProbeAt != nil && st.LastProbeAt.After(at.Add(-floor)):
		return nil
	case st.ClaimedAt != nil && !st.ClaimedAt.Before(at.Add(-ClaimLease)):
		return nil
	}
	return takeScanLimit([]*store.RunRetryState{copyRetryState(s.run)}, limit)
}
