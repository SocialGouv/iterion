package retryprobetest

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The two rules the row owes its readers, asserted on the twin so they run on
// a machine with no database. The Mongo store is pinned to this twin
// operation by operation by the ITERION_TEST_MONGO_URI-gated pairing in
// pkg/store/mongo/runs_retry_model_conformance_test.go, so a rule proved here
// is a rule the real store keeps — and a model that drifted from it fails
// there rather than here.

// armedOnAWall is the row a usage-window park leaves: armed for the instant
// the provider named, one attempt spent, no probe taken yet.
func armedOnAWall(id string, wall time.Time) *store.Run {
	after := wall
	return &store.Run{
		ID:                id,
		Status:            store.RunStatusFailedResumable,
		ContinuationState: store.ContinuationRetryArmed,
		RetryState: &store.RunRetryState{
			RetryAfter: &after,
			Wall:       &wall,
			Reason:     store.RetryReasonUsageWindow,
			Code:       "USAGE_LIMIT_BLOCKED",
			Attempts:   1,
		},
	}
}

// TestScheduleRunRetry_TheProbeLedgerBelongsToTheWall is #2451's Q2 and Q4:
// the probe budget and the probe pace bound ONE provider wall, so an arming
// that re-states the wall the row is already on keeps them, and only an
// arming that MOVES the wall starts them over.
//
// Without that, a paid park restoring the announced instant handed the same
// wait a fresh budget and a fresh pace: one wall cost up to
// max_attempts × budget probes (120 pods and 5 metered launches at the
// shipped defaults, against 24 and 0 by design).
func TestScheduleRunRetry_TheProbeLedgerBelongsToTheWall(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	wall := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	st := New(armedOnAWall("run-2451", wall))

	if pulled, err := st.PullRunRetry(ctx, "run-2451", wall, now); err != nil || !pulled {
		t.Fatalf("PullRunRetry = %v, %v; want the probe to take the row", pulled, err)
	}

	// The probed resume met the same window and its refusal named no instant
	// of its own, so the arming puts the announced instant back — for a
	// price, because the pull is older than the marker's TTL.
	ok, attempt, err := st.ScheduleRunRetry(ctx, "run-2451", wall, wall, store.RetryReasonUsageWindow, "USAGE_LIMIT_BLOCKED", 5)
	if err != nil || !ok {
		t.Fatalf("ScheduleRunRetry = %v, %d, %v; want the paid arming to land", ok, attempt, err)
	}
	row := st.Row()
	if row.ProbeCount != 1 {
		t.Errorf("probe_count = %d after an arming that re-stated the SAME wall, want the wall's own ledger kept at 1: a budget that restarts with every charged park bounds max_attempts × budget probes, not budget",
			row.ProbeCount)
	}
	if row.LastProbeAt == nil {
		t.Error("last_probe_at was dropped by an arming that re-stated the same wall: the pace belongs to the ROW, so the next probe of this wait may be taken one floor after the LAST one and not one floor after this park")
	}

	// And the complement, which is what keeps the rule a rule rather than a
	// suppression: a park on a wall the provider has MOVED is a new wait, and
	// it is probed afresh.
	moved := now.Add(7 * 24 * time.Hour).Truncate(time.Millisecond)
	if ok, _, err = st.ScheduleRunRetry(ctx, "run-2451", moved, moved, store.RetryReasonUsageWindow, "USAGE_LIMIT_BLOCKED", 5); err != nil || !ok {
		t.Fatalf("ScheduleRunRetry on a moved wall = %v, %v", ok, err)
	}
	row = st.Row()
	if row.ProbeCount != 0 {
		t.Errorf("a new wall inherited the previous wall's budget (probe_count=%d): the run would wait out a window nobody is allowed to probe", row.ProbeCount)
	}
	// And the half the budget must NOT take with it: the PACE is the row's.
	// An arming that cleared it made the row probe-eligible on the sweeper's
	// next pass (the scan's pace term treats an absent stamp as "never
	// probed"), so a provider whose refusal re-dates its window on every
	// reading was pulled once a minute, free of attempts, with nothing left
	// to bound it.
	if row.LastProbeAt == nil {
		t.Error("the arming that moved the wall dropped last_probe_at: the row is probe-eligible again on the sweeper's next pass instead of one floor later, and a refusal that re-dates its window on every reading is then pulled once per sweep tick, charged nothing and bounded by nothing")
	}
}

// TestAbandonRunRetry_RefusesARowThatMovedUnderIt is #2450's first point: the
// settlement is a conditional write, so a sweep tick that pulled the row
// forward between the caller's read and its abandon cannot be overwritten.
//
// Retiring such a row destroys the only durable copy of the provider's own
// word, days before the wall it names falls — and the run is marked final, so
// nothing lists it again.
func TestAbandonRunRetry_RefusesARowThatMovedUnderIt(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	wall := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	st := New(armedOnAWall("run-2450", wall))

	// What the caller read: the row armed on its wall.
	read := wall
	// What a sweep tick did in between: it pulled the row forward to probe it.
	if pulled, err := st.PullRunRetry(ctx, "run-2450", wall, now); err != nil || !pulled {
		t.Fatalf("PullRunRetry = %v, %v", pulled, err)
	}

	abandoned, err := st.AbandonRunRetry(ctx, "run-2450", "usage-window retries exhausted", read)
	if err != nil {
		t.Fatalf("AbandonRunRetry: %v", err)
	}
	if abandoned {
		t.Error("the settlement reported success on a row it did not own — a caller that believes the run is retired stops saying anything about it")
	}
	row := st.Row()
	if row.ProbeFrom == nil {
		t.Errorf("the abandon retired a row a sweep tick had pulled forward (read %s): the probe marker holding the provider's announced instant is gone, so the run is final days before its wall falls",
			read.Format(time.RFC3339))
	}
	if st.Doc().ContinuationState == store.ContinuationFinal {
		t.Error("the abandon closed the continuation of a row it did not own: an outcome consumer now reads a run nothing will resume, while a probe-pulled resume is in flight")
	}

	// And the complement, because "refuses" is also satisfied by "never
	// settles": the caller that reads the row as it stands retires it.
	live := st.Row().RetryAfter
	if live == nil {
		t.Fatal("the pulled row carries no armed instant")
	}
	if abandoned, err = st.AbandonRunRetry(ctx, "run-2450", "usage-window retries exhausted", *live); err != nil || !abandoned {
		t.Fatalf("AbandonRunRetry on the instant the row holds = %v, %v; want it to land — a settlement nothing can perform leaves every exhausted run saying it is still coming back", abandoned, err)
	}

	// The escape hatch: a cause about the run's own future (its pull request
	// closed) settles whatever the row holds.
	st2 := New(armedOnAWall("run-2450-closed", wall))
	if abandoned, err = st2.AbandonRunRetry(ctx, "run-2450-closed", "the pull request was closed", time.Time{}); err != nil || !abandoned {
		t.Fatalf("the unconditional settlement = %v, %v; want it to land", abandoned, err)
	}
}

// TestProbeScan_ALegacyRowIsProbedAndThenCarriesItsWall is #2450's second
// point: a row armed before the probe's marker — and before the wall — exists
// in production, and the answer has to be stated rather than discovered.
//
// What the probe does with it: it probes it. Every term of the scan's filter
// has an "absent" branch for exactly this shape (the type-bracketing trap
// ScheduleRunRetry documents), so a legacy row is probed on whatever budget
// its own ledger has left: it is pulled, its pull raises the ledger, and the
// first arming after that writes the wall and KEEPS that ledger. A row that
// never carried a wall is not a row whose wall MOVED, and the ledger it
// carries is the ledger of the wait it is already on — probe_count predates
// this field, so a row whose budget was already spent is reachable, and
// labelling its wait would otherwise buy it a second budget of pods for a
// window it had already probed to the end.
//
// Until that arming the row has no wall, which is the honest answer and not a
// silent one: store.UsageParkWall reports zero, and every reader keyed on it
// (the user notification, the operator alert, the pull request's notice) keeps
// the per-transition behaviour those rows shipped with.
func TestProbeScan_ALegacyRowIsProbedAndThenCarriesItsWall(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	wall := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)

	// The pre-#2247 shape: armed, with no marker, no ledger and no wall.
	legacy := armedOnAWall("run-legacy", wall)
	legacy.RetryState.Wall = nil
	st := New(legacy)

	if got := store.UsageParkWall(store.RunStatusFailedResumable, st.Row()); !got.IsZero() {
		t.Errorf("a row armed before the wall existed reports a wall of %s: a reader would dedup a park on an instant nobody wrote", got)
	}
	rows := st.ProbeRows(now, time.Hour, 24, 10)
	if len(rows) != 1 {
		t.Fatalf("the probe scan listed %d legacy rows, want 1: every filter term has an absent branch precisely so a row that never probed is probed", len(rows))
	}
	if pulled, err := st.PullRunRetry(ctx, "run-legacy", wall, now); err != nil || !pulled {
		t.Fatalf("PullRunRetry on a legacy row = %v, %v", pulled, err)
	}
	if row := st.Row(); row.ProbeCount != 1 || row.LastProbeAt == nil {
		t.Fatalf("the pull did not open the ledger on a legacy row: probe_count=%d last_probe_at set=%v", row.ProbeCount, row.LastProbeAt != nil)
	}
	// The scan refuses it a second time on the marker, like any other row.
	if rows := st.ProbeRows(now, time.Hour, 24, 10); len(rows) != 0 {
		t.Errorf("the probe scan listed a legacy row that already holds a marker: a second pull would overwrite the instant the first set aside")
	}

	// The first arming names the wall, and the row joins every reader keyed
	// on it — without being handed a second budget for the wait it is on.
	fresh := now.Add(7 * 24 * time.Hour).Truncate(time.Millisecond)
	if ok, _, err := st.ScheduleRunRetry(ctx, "run-legacy", fresh, fresh, store.RetryReasonUsageWindow, "USAGE_LIMIT_BLOCKED", 5); err != nil || !ok {
		t.Fatalf("ScheduleRunRetry on a legacy row = %v, %v", ok, err)
	}
	row := st.Row()
	if got := row.UsageWall(); !got.Equal(fresh) {
		t.Errorf("wall = %s after the first arming, want %s", got, fresh)
	}
	if row.ProbeCount != 1 {
		t.Errorf("probe_count = %d, want the pull this row already spent kept at 1: a row that never carried a wall is not a row whose wall MOVED, and clearing its ledger to label the wait it is already on buys that window a second budget of pods",
			row.ProbeCount)
	}
	if row.LastProbeAt == nil {
		t.Error("last_probe_at was dropped by the arming that named the wall: the pace is the row's, so this row would be probe-eligible again on the sweeper's next pass")
	}
}
