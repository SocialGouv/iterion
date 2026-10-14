package mongo

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The probe half of the retry, against a real replica set for the reason the
// file next door gives: the behaviour under test IS Mongo's. Three traps live
// here — comparison operators are type-bracketed (so every bound needs its
// "field absent" branch, or the FIRST probe of every run is refused), one
// bson.M cannot carry two $or keys, and the pull/re-arm pair is a
// compare-and-swap expressed as a conditional update.
//
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27077/?replicaSet=rs0' \
//	    devbox run -- go test ./pkg/store/mongo/ -run TestRunRetryProbe

const probeFloor = time.Hour

// TestRunRetryProbe_SeesWhatTheDueScanStructurallyCannot is the ticket's
// witness: a run armed four days out is invisible to every due pass until
// that instant, which is why a forfait an operator clears by hand is
// unobserved.
func TestRunRetryProbe_SeesWhatTheDueScanStructurallyCannot(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-parked")

	now := time.Now().UTC()
	announced := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	if scheduled, _, err := s.ScheduleRunRetry(ctx, "run-parked", announced, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil || !scheduled {
		t.Fatalf("ScheduleRunRetry = (%v, %v)", scheduled, err)
	}

	due, err := s.ListRunsDueForRetry(store.WithoutTenantFilter(ctx), now, 10)
	if err != nil {
		t.Fatalf("ListRunsDueForRetry: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("the due scan listed %d rows armed for %s — it must not, which is exactly why the probe exists", len(due), announced)
	}

	probes, err := s.ListRunsForRetryProbe(store.WithoutTenantFilter(ctx), now, probeFloor, 24, 10)
	if err != nil {
		t.Fatalf("ListRunsForRetryProbe: %v", err)
	}
	if len(probes) != 1 || probes[0].ID != "run-parked" {
		t.Fatalf("probe scan = %+v, want the one parked run", probes)
	}
	if !probes[0].RetryAfter().Equal(announced) {
		t.Errorf("probe row carries retry_after %s, want the announced %s — the pull CASes on it", probes[0].RetryAfter(), announced)
	}
}

// The pull: retry_after moves to now, the announced instant is kept aside,
// the probe ledger advances — and attempts and scheduled_at do NOT move. The
// attempt budget is for work that gets past the preflight, and the max_wait
// anchor is not a probe's to move.
func TestRunRetryProbe_PullSpendsNoAttemptAndKeepsTheAnnouncedInstant(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-pull")

	now := time.Now().UTC().Truncate(time.Millisecond)
	announced := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	if _, _, err := s.ScheduleRunRetry(ctx, "run-pull", announced, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}
	armed, err := s.LoadRun(ctx, "run-pull")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	scheduledAt := armed.RetryState.ScheduledAt

	pulled, err := s.PullRunRetry(ctx, "run-pull", announced, now)
	if err != nil || !pulled {
		t.Fatalf("PullRunRetry = (%v, %v), want (true, nil)", pulled, err)
	}

	got, err := s.LoadRun(ctx, "run-pull")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	rs := got.RetryState
	if rs == nil || rs.RetryAfter == nil || !rs.RetryAfter.Equal(now) {
		t.Fatalf("retry_after = %+v, want the probe instant %s", rs, now)
	}
	if rs.ProbeFrom == nil || !rs.ProbeFrom.Equal(announced) {
		t.Fatalf("probe_from = %v, want the announced instant %s — nothing else can put it back", rs.ProbeFrom, announced)
	}
	if rs.ProbeCount != 1 || rs.LastProbeAt == nil {
		t.Errorf("probe ledger = count %d last_probe_at %v, want 1 and a stamp — they bound and pace the probes", rs.ProbeCount, rs.LastProbeAt)
	}
	if rs.Attempts != 1 {
		t.Errorf("attempts = %d after a pull, want the 1 the arming charged: a probe that spends attempts retires the run before its wall falls", rs.Attempts)
	}
	if rs.ScheduledAt == nil || scheduledAt == nil || !rs.ScheduledAt.Equal(*scheduledAt) {
		t.Errorf("scheduled_at = %v, want the arming's %v — it anchors max_wait", rs.ScheduledAt, scheduledAt)
	}
	if got.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %v, want failed_resumable: the pull arms, it does not resume", got.Status)
	}

	// Now the due pass CAN see it, which is what makes the probe work: one
	// pass pulls, the next resumes.
	due, err := s.ListRunsDueForRetry(store.WithoutTenantFilter(ctx), time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ListRunsDueForRetry: %v", err)
	}
	if len(due) != 1 || due[0].ID != "run-pull" {
		t.Fatalf("due scan after the pull = %+v, want the pulled run", due)
	}
}

// The pull is a CAS on the instant the scan read, and it refuses a row that
// already carries a marker — a second pull would overwrite the provider's
// announced instant with a probe's own and lose it for good.
func TestRunRetryProbe_PullIsACASAndRefusesASecondMarker(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-cas")

	now := time.Now().UTC().Truncate(time.Millisecond)
	announced := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	if _, _, err := s.ScheduleRunRetry(ctx, "run-cas", announced, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}

	stale := announced.Add(-time.Hour)
	if pulled, err := s.PullRunRetry(ctx, "run-cas", stale, now); err != nil || pulled {
		t.Fatalf("PullRunRetry on a stale instant = (%v, %v), want (false, nil)", pulled, err)
	}
	if pulled, err := s.PullRunRetry(ctx, "run-cas", announced, now); err != nil || !pulled {
		t.Fatalf("PullRunRetry = (%v, %v), want (true, nil)", pulled, err)
	}
	if pulled, err := s.PullRunRetry(ctx, "run-cas", now, now.Add(time.Minute)); err != nil || pulled {
		t.Fatalf("a second pull over a live marker = (%v, %v), want (false, nil)", pulled, err)
	}
	got, err := s.LoadRun(ctx, "run-cas")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.RetryState.ProbeFrom == nil || !got.RetryState.ProbeFrom.Equal(announced) {
		t.Fatalf("probe_from = %v, want the announced instant %s intact", got.RetryState.ProbeFrom, announced)
	}
	if got.RetryState.ProbeCount != 1 {
		t.Errorf("probe_count = %d, want 1: a refused pull charges nothing", got.RetryState.ProbeCount)
	}
}

// The three bounds of the scan, each on its own row: a wait coming due within
// the floor (probing it buys nothing), a row probed within the floor (the
// pace), and a row at its budget (a forfait nobody resets stops being
// probed).
func TestRunRetryProbe_FloorAndBudgetBoundTheScan(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	now := time.Now().UTC().Truncate(time.Millisecond)

	seedFailedResumable(t, s, "run-soon")
	if _, _, err := s.ScheduleRunRetry(ctx, "run-soon", now.Add(20*time.Minute), "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}

	seedFailedResumable(t, s, "run-far")
	far := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	if _, _, err := s.ScheduleRunRetry(ctx, "run-far", far, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}

	probes, err := s.ListRunsForRetryProbe(store.WithoutTenantFilter(ctx), now, probeFloor, 24, 10)
	if err != nil {
		t.Fatalf("ListRunsForRetryProbe: %v", err)
	}
	if len(probes) != 1 || probes[0].ID != "run-far" {
		t.Fatalf("probe scan = %+v, want only run-far: a wait inside the floor is the due pass's", probes)
	}

	// Probe it, then settle it: the marker goes, the ledger stays — and the
	// pace holds, because last_probe_at is inside the floor.
	if pulled, err := s.PullRunRetry(ctx, "run-far", far, now); err != nil || !pulled {
		t.Fatalf("PullRunRetry = (%v, %v)", pulled, err)
	}
	if rearmed, err := s.RearmProbedRunRetry(ctx, "run-far", far); err != nil || !rearmed {
		t.Fatalf("RearmProbedRunRetry = (%v, %v)", rearmed, err)
	}
	probes, err = s.ListRunsForRetryProbe(store.WithoutTenantFilter(ctx), now, probeFloor, 24, 10)
	if err != nil {
		t.Fatalf("ListRunsForRetryProbe: %v", err)
	}
	if len(probes) != 0 {
		t.Fatalf("probe scan right after a probe = %+v, want none — one probe per floor per row, whatever the sweep cadence", probes)
	}
	// An hour later the pace allows it again; at the budget it stops.
	later := now.Add(probeFloor + time.Minute)
	probes, err = s.ListRunsForRetryProbe(store.WithoutTenantFilter(ctx), later, probeFloor, 24, 10)
	if err != nil {
		t.Fatalf("ListRunsForRetryProbe: %v", err)
	}
	if len(probes) != 1 {
		t.Fatalf("probe scan a floor later = %+v, want run-far again", probes)
	}
	probes, err = s.ListRunsForRetryProbe(store.WithoutTenantFilter(ctx), later, probeFloor, 1, 10)
	if err != nil {
		t.Fatalf("ListRunsForRetryProbe: %v", err)
	}
	if len(probes) != 0 {
		t.Fatalf("probe scan at the budget = %+v, want none — an operator who never resets must stop being probed", probes)
	}
	// Budget 0 is the kill switch, and it must not fall through to "every
	// never-probed row" (which the absent-field branch of the filter would).
	seedFailedResumable(t, s, "run-virgin")
	if _, _, err := s.ScheduleRunRetry(ctx, "run-virgin", far, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}
	probes, err = s.ListRunsForRetryProbe(store.WithoutTenantFilter(ctx), later, probeFloor, 0, 10)
	if err != nil {
		t.Fatalf("ListRunsForRetryProbe: %v", err)
	}
	if len(probes) != 0 {
		t.Fatalf("probe scan with the budget off = %+v, want none", probes)
	}
}

// A non-usage_window reason is not probed: the pull-forward bet is that a
// provider window may have reopened early, which says nothing about any other
// reason a retry was armed for.
func TestRunRetryProbe_ScanIsScopedToUsageWindowWaits(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-other")
	now := time.Now().UTC()
	if _, _, err := s.ScheduleRunRetry(ctx, "run-other", now.Add(48*time.Hour), "some_other_reason", "SOMETHING_ELSE", 5); err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}
	probes, err := s.ListRunsForRetryProbe(store.WithoutTenantFilter(ctx), now, probeFloor, 24, 10)
	if err != nil {
		t.Fatalf("ListRunsForRetryProbe: %v", err)
	}
	if len(probes) != 0 {
		t.Fatalf("probe scan = %+v, want none for a non-usage_window wait", probes)
	}
}

// The free re-arm: the instant moves, the marker is consumed, attempts stay —
// including when the budget is already spent, which the paid arming would
// refuse outright and turn into an abandon days early.
func TestRunRetryProbe_RearmKeepsTheAttemptsAndConsumesTheMarker(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-rearm")

	now := time.Now().UTC().Truncate(time.Millisecond)
	announced := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	// Spend the whole budget: two armings of a two-attempt policy.
	for i := 0; i < 2; i++ {
		if scheduled, _, err := s.ScheduleRunRetry(ctx, "run-rearm", announced, "usage_window", "USAGE_LIMIT_BLOCKED", 2); err != nil || !scheduled {
			t.Fatalf("ScheduleRunRetry %d = (%v, %v)", i, scheduled, err)
		}
	}
	if scheduled, _, err := s.ScheduleRunRetry(ctx, "run-rearm", announced, "usage_window", "USAGE_LIMIT_BLOCKED", 2); err != nil || scheduled {
		t.Fatalf("a third paid arming = (%v, %v), want refused — the fixture needs the budget spent", scheduled, err)
	}
	if pulled, err := s.PullRunRetry(ctx, "run-rearm", announced, now); err != nil || !pulled {
		t.Fatalf("PullRunRetry = (%v, %v)", pulled, err)
	}

	fresh := announced.Add(2 * time.Hour).Truncate(time.Millisecond)
	rearmed, err := s.RearmProbedRunRetry(ctx, "run-rearm", fresh)
	if err != nil || !rearmed {
		t.Fatalf("RearmProbedRunRetry = (%v, %v), want (true, nil) with the budget spent", rearmed, err)
	}
	got, err := s.LoadRun(ctx, "run-rearm")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	rs := got.RetryState
	if rs.RetryAfter == nil || !rs.RetryAfter.Equal(fresh) {
		t.Fatalf("retry_after = %v, want the provider's fresh instant %s", rs.RetryAfter, fresh)
	}
	if rs.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 — a probe's re-failure is not an attempt", rs.Attempts)
	}
	if rs.ProbeFrom != nil {
		t.Errorf("probe_from = %v, want consumed: the run's NEXT failure is its own, and must be charged", rs.ProbeFrom)
	}
	if rs.ProbeCount != 1 || rs.LastProbeAt == nil {
		t.Errorf("probe ledger = count %d last_probe_at %v, want 1 and a stamp kept: a ledger a probe could reset would bound nothing", rs.ProbeCount, rs.LastProbeAt)
	}
	if got.ContinuationState != store.ContinuationRetryArmed {
		t.Errorf("continuation_state = %q, want retry_armed", got.ContinuationState)
	}
	if rearmed, err := s.RearmProbedRunRetry(ctx, "run-rearm", fresh); err != nil || rearmed {
		t.Fatalf("a second free re-arm = (%v, %v), want (false, nil): the marker is consumed", rearmed, err)
	}
}

// A PAID arming resets the probe ledger: the budget is per wait, and a paid
// arming normally opens a new one. (A probe's own re-arming must NOT — see
// above.) It resets it even when the instant armed is the SAME announced one —
// as here — which is why the bound on one wait is max_attempts × budget:
// store.ProbeMarkerTTL states that cost.
func TestRunRetryProbe_APaidArmingResetsTheLedger(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-ledger")

	now := time.Now().UTC().Truncate(time.Millisecond)
	announced := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	if _, _, err := s.ScheduleRunRetry(ctx, "run-ledger", announced, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}
	if pulled, err := s.PullRunRetry(ctx, "run-ledger", announced, now); err != nil || !pulled {
		t.Fatalf("PullRunRetry = (%v, %v)", pulled, err)
	}
	if _, _, err := s.ScheduleRunRetry(ctx, "run-ledger", announced, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("paid arming after a probe: %v", err)
	}
	got, err := s.LoadRun(ctx, "run-ledger")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	rs := got.RetryState
	if rs.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", rs.Attempts)
	}
	if rs.ProbeFrom != nil || rs.ProbeCount != 0 || rs.LastProbeAt != nil {
		t.Errorf("probe ledger = %+v, want cleared by a paid arming: the budget is per armed wait", rs)
	}
}

// The counter-intuitive one, and load-bearing: EVERY resume clears the retry
// (cloudpublisher + the sweeper's own tail), and it runs before the pod
// reaches the usage preflight. So the clear must keep the probe ledger —
// clearing probe_from would delete the marker exactly in the case it exists
// for, and clearing probe_count would hand every probe a fresh budget.
func TestRunRetryProbe_TheMarkerSurvivesTheResumesClear(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-clear")

	now := time.Now().UTC().Truncate(time.Millisecond)
	announced := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	if _, _, err := s.ScheduleRunRetry(ctx, "run-clear", announced, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}
	if pulled, err := s.PullRunRetry(ctx, "run-clear", announced, now); err != nil || !pulled {
		t.Fatalf("PullRunRetry = (%v, %v)", pulled, err)
	}
	if err := s.ClearRunRetry(ctx, "run-clear"); err != nil {
		t.Fatalf("ClearRunRetry: %v", err)
	}
	got, err := s.LoadRun(ctx, "run-clear")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	rs := got.RetryState
	if rs.RetryAfter != nil {
		t.Errorf("retry_after = %v, want disarmed by the clear", rs.RetryAfter)
	}
	if rs.ProbeFrom == nil || !rs.ProbeFrom.Equal(announced) {
		t.Fatalf("probe_from = %v after the resume's clear, want the announced %s — the runner reads it AFTER this write to know its re-failure was a probe's",
			rs.ProbeFrom, announced)
	}
	if rs.ProbeCount != 1 {
		t.Errorf("probe_count = %d after the clear, want 1 kept: a budget reset by each probe bounds nothing", rs.ProbeCount)
	}
}

// Abandoning ends the wait, so the marker goes with it: otherwise the next
// arming of a hand-resumed run would be free.
func TestRunRetryProbe_AbandonDropsTheMarker(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-abandon-probe")

	now := time.Now().UTC().Truncate(time.Millisecond)
	announced := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	if _, _, err := s.ScheduleRunRetry(ctx, "run-abandon-probe", announced, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}
	if pulled, err := s.PullRunRetry(ctx, "run-abandon-probe", announced, now); err != nil || !pulled {
		t.Fatalf("PullRunRetry = (%v, %v)", pulled, err)
	}
	if err := s.AbandonRunRetry(ctx, "run-abandon-probe", "auto-retry abandoned: the bot is gone"); err != nil {
		t.Fatalf("AbandonRunRetry: %v", err)
	}
	got, err := s.LoadRun(ctx, "run-abandon-probe")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.RetryState.ProbeFrom != nil {
		t.Fatalf("probe_from = %v after an abandon, want cleared", got.RetryState.ProbeFrom)
	}
	if rearmed, err := s.RearmProbedRunRetry(ctx, "run-abandon-probe", announced); err != nil || rearmed {
		t.Fatalf("RearmProbedRunRetry on an abandoned row = (%v, %v), want (false, nil)", rearmed, err)
	}
}

// A run an operator resumed (or cancelled) is not re-armed behind their back:
// the status is in the filter, like every other write in this file.
func TestRunRetryProbe_RearmRefusesARunThatIsNoLongerResumable(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-resumed-probe")

	now := time.Now().UTC().Truncate(time.Millisecond)
	announced := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)
	if _, _, err := s.ScheduleRunRetry(ctx, "run-resumed-probe", announced, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}
	if pulled, err := s.PullRunRetry(ctx, "run-resumed-probe", announced, now); err != nil || !pulled {
		t.Fatalf("PullRunRetry = (%v, %v)", pulled, err)
	}
	if err := s.UpdateRunStatus(ctx, "run-resumed-probe", store.RunStatusRunning, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	if rearmed, err := s.RearmProbedRunRetry(ctx, "run-resumed-probe", announced); err != nil || rearmed {
		t.Fatalf("RearmProbedRunRetry on a running run = (%v, %v), want (false, nil)", rearmed, err)
	}
	if pulled, err := s.PullRunRetry(ctx, "run-resumed-probe", now, now.Add(time.Minute)); err != nil || pulled {
		t.Fatalf("PullRunRetry on a running run = (%v, %v), want (false, nil)", pulled, err)
	}
}
