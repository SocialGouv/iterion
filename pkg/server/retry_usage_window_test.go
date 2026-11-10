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
// due pass and the notice through retrypolicy.UsageWindowNotBefore.

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

// The replay: the sweeper's own passes, on the ticks bko#82 was relaunched
// at, pull nothing forward and resume nothing; on the tick after the wall
// the due pass resumes it.
func TestUsageWindow_Bko82IsNotRelaunchedBeforeItsWall(t *testing.T) {
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

// The escape hatch: an operator who tops up forfaits by hand and wants it seen
// within the hour opts back into probing before the window.
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

// A row due before its window — re-armed after a failed resume, pulled by a
// probe whose marker has aged out — is held until the window, free of
// charge, instead of relaunched into the wall.
func TestUsageWindow_TheDuePassHoldsARowItsWindowStillHolds(t *testing.T) {
	st := newFakeRetryStore()
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)
	now := time.Now().UTC()
	wall, parked := now.Add(4*24*time.Hour), now.Add(-time.Hour)
	ref := dueRef("run-reamed", now.Add(-time.Minute))
	ref.RetryState.Wall = &wall
	ref.RetryState.ScheduledAt = &parked
	run := ref.Record()
	run.Status = store.RunStatusFailedResumable
	run.FailureCode = store.FailureUsageLimitBlocked
	st.loadRun[ref.ID] = run
	st.claimWins[ref.ID] = true

	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{ref}}, resumer, now)

	if len(resumer.calls) != 0 {
		t.Fatalf("resumed %v — the record names a wall at %s, and a resume before it only meets it again", resumer.calls, wall.Format(time.RFC3339))
	}
	if got, ok := st.rearmed[ref.ID]; !ok || !got.Equal(wall) {
		t.Fatalf("held until %v (%v), want the wall %s", got, ok, wall.Format(time.RFC3339))
	}
	if len(st.armBudget) != 0 {
		t.Errorf("the hold charged an attempt (%v) — nothing reached the provider", st.armBudget)
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

// The notice promises the instant the resumer keeps: the armed retry, never
// earlier than the window.
func TestUsageWindow_ThePauseNoticeNamesTheWindowTheResumerWaitsFor(t *testing.T) {
	now := time.Date(2026, 11, 10, 3, 58, 0, 0, time.UTC)
	soon, wall := now.Add(time.Hour), bkoWall
	run := &store.Run{
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
	body := gatePauseNoticeBody(run, pauseNoticeRoleReviewer, now)
	if want := wall.Format("15:04 UTC on 2006-01-02"); !strings.Contains(body, want) {
		t.Fatalf("notice = %q, want the resume promised at %s — the sweeper holds the run until its wall", body, want)
	}
}
