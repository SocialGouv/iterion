package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// One window, every reader (#3127). bko#82 parked at 03:58Z on z.ai's weekly
// [1310] wall; the pull-request notice promised a resume at 17:25Z on 14/11,
// and the sweeper relaunched the run at 04:14, 04:58 and 05:59Z — each attempt
// USAGE_LIMIT_BLOCKED, each spending a pod and ten of the run's active
// minutes. The window the record names is now read by the probe pass, the
// due pass and the notice through retrypolicy.UsageWindowNotBefore, and a
// wait it holds is probed through its credential window only — one run per
// credential per floor (retry_probe_credential_test.go).

var (
	bkoParkedAt = time.Date(2026, 11, 10, 3, 58, 0, 0, time.UTC)
	bkoWall     = time.Date(2026, 11, 14, 17, 24, 0, 0, time.UTC)
)

// bkoParkedRef is bko#82's row as the scans list it: armed a minute past the
// provider's dated wall, the launch's skipped credential reopening on the same
// wall, the arming settled.
func bkoParkedRef(id string) mongostore.RetryDueRef {
	ref := dueRef(id, bkoWall.Add(time.Minute))
	wall, parked, skipped := bkoWall, bkoParkedAt, bkoWall.Add(30*time.Second)
	ref.RetryState.Wall = &wall
	ref.RetryState.ScheduledAt = &parked
	ref.Status = store.RunStatusFailedResumable
	ref.FailureCode = store.FailureUsageLimitBlocked
	ref.ContinuationState = store.ContinuationRetryArmed
	ref.SkippedCredReopensAt = &skipped
	return ref
}

// The replay, on a record that names no credential (ambient credentials, a
// run stamped before the field): no credential window speaks for it, so the
// sweeper's own passes, on the ticks bko#82 was relaunched at, pull nothing
// forward and resume nothing before its wall.
func TestUsageWindow_ARunNamingNoCredentialIsNotRelaunchedBeforeItsWall(t *testing.T) {
	st := newFakeRetryStore()
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)
	ref := bkoParkedRef("bko-82")
	st.loadRun[ref.ID] = ref.Record()
	st.claimWins[ref.ID] = true

	for _, tick := range []time.Time{
		bkoParkedAt.Add(time.Minute),
		time.Date(2026, 11, 10, 4, 14, 0, 0, time.UTC),
		time.Date(2026, 11, 10, 4, 58, 0, 0, time.UTC),
		time.Date(2026, 11, 10, 5, 59, 0, 0, time.UTC),
		time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC),
	} {
		s.sweepRetryProbes(context.Background(), &fakeRetryLister{probeRefs: []mongostore.RetryDueRef{ref}}, tick)
		if len(st.pulls) != 0 {
			t.Fatalf("at %s the probe pass pulled %v forward — its wall is dated %s, and a pull relaunches the run into it",
				tick.Format(time.RFC3339), st.pulls, bkoWall.Format(time.RFC3339))
		}
	}
	if len(resumer.calls) != 0 {
		t.Fatalf("resumed %d times before the wall", len(resumer.calls))
	}
}

// The per-run escape hatch: every wait is probed on its own, before its
// window, whatever credential window it belongs to.
func TestUsageWindow_TheOperatorMayProbeBeforeTheWindow(t *testing.T) {
	t.Setenv(EnvRetryProbeBeforeWindow, "true")
	st := newFakeRetryStore()
	s := newRetrySweeperServer(t, st, nil)
	ref := bkoParkedRef("bko-82")
	tick := bkoParkedAt.Add(time.Minute)
	s.sweepRetryProbes(context.Background(), &fakeRetryLister{probeRefs: []mongostore.RetryDueRef{ref}}, tick)
	if len(st.pulls) != 1 {
		t.Fatalf("pulls = %v, want the row pulled — %s=true is the operator's explicit choice to look early", st.pulls, EnvRetryProbeBeforeWindow)
	}
}

// The probe pass reads past the waits its window holds: they sort first on
// retry_after, and reading only the batch would starve the wait behind them.
func TestUsageWindow_HeldWaitsDoNotStarveTheProbe(t *testing.T) {
	st := newFakeRetryStore()
	s := newRetrySweeperServer(t, st, nil)
	tick := bkoParkedAt.Add(time.Minute)
	held := []mongostore.RetryDueRef{bkoParkedRef("held-1"), bkoParkedRef("held-2"), bkoParkedRef("held-3")}
	// A wait whose record names no window: the arming's cadence is all there
	// is, and probing keeps it (no indefinite park).
	open := dueRef("unreadable", bkoWall.Add(2*time.Hour))
	open.Status = store.RunStatusFailedResumable
	s.sweepRetryProbes(context.Background(), &fakeRetryLister{probeRefs: append(held, open)}, tick)
	if len(st.pulls) != 1 || st.pulls[0].runID != "unreadable" {
		t.Fatalf("pulls = %v, want only the wait with no readable window", st.pulls)
	}
}

// A row due before its window — pulled forward by a probe whose resume never
// came, its pull aged past the marker's TTL — is held until the window, free
// of charge, instead of relaunched into the wall. It comes back the way its
// arming would have: the come-back pad past the window, spread by the run's
// jitter, never on the instant the wall falls.
func TestUsageWindow_TheDuePassHoldsARowItsWindowStillHolds(t *testing.T) {
	st := newFakeRetryStore()
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)
	now := time.Now().UTC()
	wall, parked := now.Add(4*24*time.Hour), now.Add(-3*time.Hour)
	announced, pulledAt := wall.Add(time.Minute), now.Add(-store.ProbeMarkerTTL-time.Minute)
	const jitter = 20 * time.Minute
	// Five rows on one wall, every pull aged out: what a sweeper that was
	// down for an hour finds.
	var refs []mongostore.RetryDueRef
	for _, id := range []string{"run-1", "run-2", "run-3", "run-4", "run-5"} {
		ref := dueRef(id, pulledAt)
		ref.RetryState.Wall = &wall
		ref.RetryState.ScheduledAt = &parked
		ref.RetryState.ProbeFrom = &announced
		ref.RetryState.LastProbeAt = &pulledAt
		run := ref.Record()
		run.Status = store.RunStatusFailedResumable
		run.FailureCode = store.FailureUsageLimitBlocked
		run.RetryPolicy = &store.RunRetryPolicy{Jitter: jitter.String()}
		st.loadRun[ref.ID] = run
		st.claimWins[ref.ID] = true
		refs = append(refs, ref)
	}

	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: refs}, resumer, now)

	if len(resumer.calls) != 0 {
		t.Fatalf("resumed %v — the record names a wall at %s, and a resume before it only meets it again", resumer.calls, wall.Format(time.RFC3339))
	}
	wakes := map[time.Time]bool{}
	earliest := wall.Add(time.Minute)
	for _, ref := range refs {
		got, ok := st.rearmed[ref.ID]
		if !ok || got.Before(earliest) || !got.Before(earliest.Add(jitter)) {
			t.Fatalf("%s held until %v (%v), want within [%s, %s): the come-back pad past the wall, spread by the run's %s jitter",
				ref.ID, got, ok, earliest.Format(time.RFC3339), earliest.Add(jitter).Format(time.RFC3339), jitter)
		}
		wakes[got] = true
	}
	if len(wakes) < 2 {
		t.Fatalf("five rows held to one wall all wake at %v — unjittered, they resume together and can exhaust the fresh window at once", wakes)
	}
	if len(st.armBudget) != 0 {
		t.Errorf("the hold charged an attempt (%v) — nothing reached the provider", st.armBudget)
	}
}

// A run released on its credential window's answer is not held, however long
// after the release its resume comes: the shared circuit deferred it past the
// marker's TTL, so the pull no longer reads as in flight — but the window's
// probe got past the wall, and that answer travels with the row
// (RunRetryState.ReleasedBy). Held to the date, the run would sit out the
// whole wall, and it is no longer a wait a later answer could release.
func TestUsageWindow_AReleasedRunIsNotHeldHoweverLateItsResume(t *testing.T) {
	st := newFakeRetryStore()
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)
	now := time.Now().UTC()
	wall, parked := now.Add(4*24*time.Hour), now.Add(-5*time.Hour)
	announced, releasedAt := wall.Add(time.Minute), now.Add(-store.ProbeMarkerTTL-time.Hour)
	ref := dueRef("run-released", now.Add(-time.Minute))
	ref.RetryState.Wall = &wall
	ref.RetryState.ScheduledAt = &parked
	ref.RetryState.ProbeFrom = &announced
	ref.RetryState.LastProbeAt = &releasedAt
	ref.RetryState.ProbeCount = 1
	ref.RetryState.ReleasedBy = "run-canary"
	run := ref.Record()
	run.Status = store.RunStatusFailedResumable
	run.FailureCode = store.FailureUsageLimitBlocked
	st.loadRun[ref.ID] = run
	st.claimWins[ref.ID] = true
	if ref.RetryState.ProbeInFlight(now) {
		t.Fatal("the release reads as a pull in flight — the premise of this witness is a resume deferred past the marker's TTL")
	}

	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{ref}}, resumer, now)

	if len(resumer.calls) != 1 {
		held, ok := st.rearmed[ref.ID]
		t.Fatalf("resumes = %d (held until %v: %v), want the released run resumed — its window's probe got past the wall, and holding it to %s undoes that answer",
			len(resumer.calls), held, ok, wall.Format(time.RFC3339))
	}
}

// Unjittered, the hold still comes back just past the wall, never on it: a
// resume a second early, or against a provider clock a little behind ours,
// meets the wall — one paid attempt and one pod, with no marker to make it
// free.
func TestUsageWindow_TheHoldComesBackJustPastTheWall(t *testing.T) {
	st := newFakeRetryStore()
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)
	now := time.Now().UTC()
	wall, parked := now.Add(4*24*time.Hour), now.Add(-3*time.Hour)
	announced, pulledAt := wall.Add(time.Minute), now.Add(-store.ProbeMarkerTTL-time.Minute)
	ref := dueRef("run-pulled", pulledAt)
	ref.RetryState.Wall = &wall
	ref.RetryState.ScheduledAt = &parked
	ref.RetryState.ProbeFrom = &announced
	ref.RetryState.LastProbeAt = &pulledAt
	run := ref.Record()
	run.Status = store.RunStatusFailedResumable
	run.FailureCode = store.FailureUsageLimitBlocked
	run.RetryPolicy = &store.RunRetryPolicy{Jitter: "0s"}
	st.loadRun[ref.ID] = run
	st.claimWins[ref.ID] = true

	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{ref}}, resumer, now)

	if got, ok := st.rearmed[ref.ID]; !ok || !got.Equal(wall.Add(time.Minute)) {
		t.Fatalf("held until %v (%v), want %s — a minute past the wall %s",
			got, ok, wall.Add(time.Minute).Format(time.RFC3339), wall.Format(time.RFC3339))
	}
}

// The wall a row carries is the wait's dedup identity, and a park that
// brought no dated news keeps the one the row already holds — one that can
// belong to another credential, or to a window the run already got past.
// The instant the arming chose is the fact of THIS park: a row due at it is
// resumed, never held to a kept wall days further out (the team key that
// reopens this afternoon while a forfait it no longer runs on is walled
// until Monday).
func TestUsageWindow_TheDuePassResumesARowAtTheInstantItsArmingChose(t *testing.T) {
	st := newFakeRetryStore()
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)
	now := time.Now().UTC()
	keptWall, parked := now.Add(4*24*time.Hour), now.Add(-5*time.Hour)
	ref := dueRef("run-kept-wall", now.Add(-time.Minute))
	ref.RetryState.Wall = &keptWall
	ref.RetryState.ScheduledAt = &parked
	run := ref.Record()
	run.Status = store.RunStatusFailedResumable
	run.FailureCode = store.FailureUsageLimitBlocked
	st.loadRun[ref.ID] = run
	st.claimWins[ref.ID] = true

	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{ref}}, resumer, now)

	if len(resumer.calls) != 1 {
		t.Fatalf("Resume calls = %d (held until %v), want 1 — the arming chose %s, and the wall %s away is one the row kept, not this park's",
			len(resumer.calls), st.rearmed[ref.ID], ref.RetryAfter().Format(time.RFC3339), time.Until(keptWall).Round(time.Hour))
	}
}

// The run's own max_wait still bounds the hold: past the horizon measured from
// its last paid park, the due pass resumes and checks again, as the operator
// asked — a window further out than the horizon never parks a run beyond it.
func TestUsageWindow_TheMaxWaitHorizonStillWakesTheRun(t *testing.T) {
	st := newFakeRetryStore()
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)
	now := time.Now().UTC()
	wall, parked := now.Add(48*time.Hour), now.Add(-37*time.Hour)
	ref := dueRef("run-horizon", now.Add(-time.Minute))
	ref.RetryState.Wall = &wall
	ref.RetryState.ScheduledAt = &parked
	run := ref.Record()
	run.Status = store.RunStatusFailedResumable
	run.FailureCode = store.FailureUsageLimitBlocked
	run.RetryPolicy = &store.RunRetryPolicy{MaxWait: "36h"}
	st.loadRun[ref.ID] = run
	st.claimWins[ref.ID] = true

	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{ref}}, resumer, now)

	if len(resumer.calls) != 1 {
		t.Fatalf("Resume calls = %d, want 1 — the run's 36h max_wait from its park %s has passed", len(resumer.calls), parked.Format(time.RFC3339))
	}
}

// The notice promises the instant the resumer keeps: the armed retry — a
// wall the row kept from another park does not move it — and, for a row its
// window holds past the armed instant (a pull whose resume aged out), the
// come-back past that window, which the due pass holds it to.
func TestUsageWindow_ThePauseNoticeNamesTheWindowTheResumerWaitsFor(t *testing.T) {
	now := time.Date(2026, 11, 10, 3, 58, 0, 0, time.UTC)
	soon, wall := now.Add(time.Hour), bkoWall
	kept := &store.Run{
		ID:          "bko-82",
		Status:      store.RunStatusFailedResumable,
		FailureCode: store.FailureUsageLimitBlocked,
		RetryState: &store.RunRetryState{
			RetryAfter:  &soon,
			Wall:        &wall,
			ScheduledAt: &now,
			Reason:      store.RetryReasonUsageWindow,
			Attempts:    1,
		},
	}
	body := gatePauseNoticeBody(kept, pauseNoticeRoleReviewer, now)
	if want := soon.Format("15:04 UTC on 2006-01-02"); !strings.Contains(body, want) {
		t.Fatalf("notice = %q, want the resume promised at %s — the instant the arming chose, which the sweeper keeps", body, want)
	}

	pulledAt, announced := now.Add(-2*time.Hour), wall.Add(time.Minute)
	pulled := &store.Run{
		ID:          "bko-82",
		Status:      store.RunStatusFailedResumable,
		FailureCode: store.FailureUsageLimitBlocked,
		RetryState: &store.RunRetryState{
			RetryAfter:  &pulledAt,
			ProbeFrom:   &announced,
			LastProbeAt: &pulledAt,
			Wall:        &wall,
			ScheduledAt: &pulledAt,
			Reason:      store.RetryReasonUsageWindow,
			Attempts:    1,
		},
	}
	body = gatePauseNoticeBody(pulled, pauseNoticeRoleReviewer, now)
	if want := wall.Add(time.Minute).Format("15:04 UTC on 2006-01-02"); !strings.Contains(body, want) {
		t.Fatalf("notice = %q, want the resume promised at %s — the sweeper holds the run until its wall", body, want)
	}
}
