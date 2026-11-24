package alert_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// One park, one instant, whoever tells it (#3127). A probe's pull moved this
// row's retry to its own instant and its resume never came: the due pass
// holds the run to its window — the come-back past the wall — and every
// announcement of the park must name that instant, not the pull's stale
// retry_after. The owner's notification reads it from the run-outcome
// payload, the operator from the ops alert, the pull request from the gate
// notice; all three read retrypolicy.UsageParkResumesAt.
func TestOpsDispatcher_AUsageParkIsAnnouncedAtTheInstantEveryReaderSays(t *testing.T) {
	d, rs, sink := opsWorld(t)
	now := time.Now().UTC().Truncate(time.Second)
	wall, parked := now.Add(72*time.Hour), now.Add(-3*time.Hour)
	pulledAt, announced := now.Add(-store.ProbeMarkerTTL-time.Minute), wall.Add(time.Minute)
	run := seedOpsRun(t, rs, store.RunStatusFailedResumable, func(r *store.Run) {
		r.FailureCode = store.FailureUsageLimitBlocked
		r.ContinuationState = store.ContinuationRetryArmed
		r.RetryPolicy = &store.RunRetryPolicy{Jitter: "0s"}
		r.RetryState = &store.RunRetryState{
			RetryAfter: &pulledAt, ProbeFrom: &announced, LastProbeAt: &pulledAt,
			Wall: &wall, ScheduledAt: &parked,
			Reason: store.RetryReasonUsageWindow, Code: "USAGE_LIMIT_BLOCKED", Attempts: 1,
		}
	})
	doc, err := rs.LoadRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := retrypolicy.UsageParkResumesAt(doc, now)
	if !want.Equal(wall.Add(time.Minute)) {
		t.Fatalf("UsageParkResumesAt = %s, want %s — the premise: the window holds the run past its pulled retry_after", want, wall.Add(time.Minute))
	}

	ev := trigger.BuildRunOutcome(context.Background(), rs, run.ID, nil)
	if got, _ := ev.Payload["usage_park_resumes_at"].(string); got != want.Format(time.RFC3339) {
		t.Errorf("run-outcome usage_park_resumes_at = %q, want %q — the owner is told an instant the due pass does not keep", got, want.Format(time.RFC3339))
	}
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	alerts := sink.alerts()
	if len(alerts) != 1 {
		t.Fatalf("%d alerts, want 1", len(alerts))
	}
	if !strings.Contains(alerts[0].Reason, "automatic retry armed for "+want.Format(time.RFC3339)) {
		t.Errorf("ops alert reason = %q, want the retry announced for %s", alerts[0].Reason, want.Format(time.RFC3339))
	}
}
