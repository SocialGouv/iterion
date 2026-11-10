package runner

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/retryprobetest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The wall a row carries is its wait's dedup identity, and a paid park that
// brings no dated news keeps the one the row already holds
// (usageParkWall). Such a kept wall can belong to a credential the run no
// longer waits on, or to a window the run already got past — and every
// automatic re-launcher reads the window through retrypolicy's chokepoint,
// so a kept wall read as "the earliest instant the run can progress" held
// the run days past the instant its own arming chose. Driven through the
// REAL arming on the store twin (internal/retryprobetest), which keeps the
// wall the way Mongo's update does.

// keptWallRun is a run parked on a usage window, its policy unjittered so the
// instants compare exactly.
func keptWallRun(id string) *store.Run {
	return &store.Run{
		ID:          id,
		Status:      store.RunStatusFailedResumable,
		FailureCode: store.FailureUsageLimitBlocked,
		RetryPolicy: &store.RunRetryPolicy{UsageWindow: "resume", MaxAttempts: 5, MaxWait: "192h", Jitter: "0s"},
	}
}

// weeklyWallAt is z.ai's dated weekly refusal ([1310]), its reset at `at`.
func weeklyWallAt(at time.Time) error {
	return &runtime.RuntimeError{
		Code: runtime.ErrCodeUsageLimitBlocked,
		Message: `node "review" execution failed: rate_limited (claw): [1310][Weekly/Monthly Limit Exhausted. Your limit will reset at ` +
			at.In(facadeClock).Format("2006-01-02 15:04:05") + `][20261110035800aaaa]`,
	}
}

// assertTheArmingsInstantHolds checks every reading of the park against the
// instant the arming chose: the window never holds the run past it, every
// announcement names it, and the due pass, at that instant, resumes.
func assertTheArmingsInstantHolds(t *testing.T, doc *store.Run, now time.Time) {
	t.Helper()
	armed := doc.RetryState.RetryAfter.UTC()
	if notBefore, waits := retrypolicy.UsageWindowNotBefore(doc, now); waits && notBefore.After(armed) {
		t.Fatalf("the window holds the run until %s (wall %s kept by the park), past the %s its own arming chose — the due pass would defer it there",
			notBefore.Format(time.RFC3339), doc.RetryState.Wall.Format(time.RFC3339), armed.Format(time.RFC3339))
	}
	if got := retrypolicy.UsageParkResumesAt(doc, now); !got.Equal(armed) {
		t.Fatalf("announced resume = %s, want the armed %s", got.Format(time.RFC3339), armed.Format(time.RFC3339))
	}
	if _, waits := retrypolicy.UsageWindowNotBefore(doc, armed); waits {
		t.Fatalf("at its armed instant %s the run is still held", armed.Format(time.RFC3339))
	}
}

// Park 1 is on the platform forfait's dated weekly wall, the team key the
// launch passed over reopening this afternoon: armed for the team key.
// The resume lands on the team key, the run works, and the team key's five
// hours shut again — rendered from a duration, no news about a wall, so the
// row keeps the forfait's weekly wall. The run waits on the team key's five
// hours, not on the forfait's Monday.
func TestUsageWindow_AWallKeptFromAnotherCredentialDoesNotHoldTheRun(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	weekly, teamKey := now.Add(96*time.Hour), now.Add(5*time.Hour)
	run := keptWallRun("run-forfait")
	run.SkippedCredReopensAt = &teamKey
	st := retryprobetest.New(run)
	r := &Runner{cfg: Config{Store: st}}
	quiet := iterlog.New(iterlog.LevelError, io.Discard)

	if got := r.armUsageWindowRetry(context.Background(), weeklyWallAt(weekly), run.ID, spentNothing, quiet); got != usageRetryArmed {
		t.Fatalf("park 1 outcome = %v, want armed", got)
	}
	if row := st.Row(); !row.RetryAfter.Equal(teamKey.Add(time.Minute)) || !row.Wall.Equal(weekly) {
		t.Fatalf("park 1 row = retry_after %v wall %v, want the team key's reopening + 1m and the weekly wall %s", row.RetryAfter, row.Wall, weekly)
	}

	// The resume: the retry is cleared, the launch passes over the forfait
	// (its reopening is the weekly wall), and the run works on the team key.
	if err := st.ClearRunRetry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	st.Mutate(func(doc *store.Run) { doc.SkippedCredReopensAt = &weekly })
	if got := r.armUsageWindowRetry(context.Background(), facadeWindowErr(now), run.ID, spentTokens, quiet); got != usageRetryArmed {
		t.Fatalf("park 2 outcome = %v, want armed", got)
	}
	doc := st.Doc()
	if !doc.RetryState.Wall.Equal(weekly) {
		t.Fatalf("park 2 wall = %v, want the weekly wall kept (a duration rendering is no news) — the premise of this witness", doc.RetryState.Wall)
	}
	assertTheArmingsInstantHolds(t, doc, now)
}

// An operator tops up the forfait and resumes by hand; the resume keeps the
// wall the row holds. The run works, then meets a refusal that names no
// instant: the blind wait is what its arming chose, and the wall it already
// got past holds it no longer.
func TestUsageWindow_AWallTheRunGotPastDoesNotHoldItsNextPark(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gotPast, parked := now.Add(72*time.Hour), now.Add(-time.Hour)
	run := keptWallRun("run-topped-up")
	run.RetryState = &store.RunRetryState{
		Wall: &gotPast, ScheduledAt: &parked, Attempts: 1,
		Reason: store.RetryReasonUsageWindow, Code: string(runtime.ErrCodeUsageLimitBlocked),
	}
	st := retryprobetest.New(run)
	r := &Runner{cfg: Config{Store: st}}

	if got := r.armUsageWindowRetry(context.Background(), blindUsageWindowErr(), run.ID, spentTokens, iterlog.New(iterlog.LevelError, io.Discard)); got != usageRetryArmed {
		t.Fatalf("outcome = %v, want armed", got)
	}
	doc := st.Doc()
	if !doc.RetryState.Wall.Equal(gotPast) {
		t.Fatalf("wall = %v, want the one the row held kept — the premise of this witness", doc.RetryState.Wall)
	}
	assertTheArmingsInstantHolds(t, doc, now)
}
