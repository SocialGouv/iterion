package cli

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The in-process auto-resume loop is an automatic re-launcher too (#3127): a
// run whose record names a usage window it waits for is not resumed before
// it, whatever the refusal in hand says. The loop announces its wait on the
// timeline before it sleeps, so the witness reads that announcement and
// cancels the sleep.
func TestAutoResumeLoop_WaitsForTheWindowTheRecordNames(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const runID = "run-local-window"
	run, err := s.CreateRun(ctx, runID, "w", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	wall, parked := now.Add(3*time.Hour), now
	run.Status = store.RunStatusFailedResumable
	run.FailureCode = store.FailureUsageLimitBlocked
	run.RetryState = &store.RunRetryState{RetryAfter: &wall, Wall: &wall, ScheduledAt: &parked, Reason: store.RetryReasonUsageWindow}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	// The refusal in hand names a reset two minutes out; the record's wall
	// is three hours out.
	execErr := wrapAsRuntime(&delegate.ErrRateLimited{Kind: delegate.RateLimitKindUsageWindow, ResetAt: now.Add(2 * time.Minute)})
	loopCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	cfg := autoResumeConfig{MaxAttempts: 1, Retry: defaultRetryPolicy()}
	_ = autoResumeLoop(loopCtx, nil, s, runID, cfg, execErr, iterlog.New(iterlog.LevelError, nil))

	events, err := s.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != store.EventRunAutoResumed {
			continue
		}
		ms, _ := ev.Data["delay_ms"].(float64)
		if got := time.Duration(ms) * time.Millisecond; got < 2*time.Hour+59*time.Minute {
			t.Fatalf("the loop waits %s before resuming, want ~3h — the record's wall at %s, not the two-minute reset in the refusal", got, wall.Format(time.RFC3339))
		}
		return
	}
	t.Fatal("the loop announced no wait on the timeline")
}
