package mongo

import (
	"testing"
	"time"
)

func TestRetryCircuitFailureOpensAndSuccessCloses(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	now := time.Now().UTC().Truncate(time.Millisecond)

	for attempt := 1; attempt <= 3; attempt++ {
		state, err := s.RecordRetryFailure(ctx, "workflow:rev-1", "run-a", now, 3, 15*time.Minute)
		if err != nil {
			t.Fatalf("RecordRetryFailure %d: %v", attempt, err)
		}
		if state.ConsecutiveFailures != attempt {
			t.Fatalf("failure count = %d, want %d", state.ConsecutiveFailures, attempt)
		}
		if attempt < 3 && state.OpenUntil != nil {
			t.Fatalf("circuit opened before threshold: %+v", state)
		}
	}

	open, err := s.RetryCircuitOpen(ctx, "workflow:rev-1", now)
	if err != nil || open == nil || open.OpenUntil == nil {
		t.Fatalf("RetryCircuitOpen = (%+v, %v), want open", open, err)
	}
	if err := s.RecordRetrySuccess(ctx, "workflow:rev-1", now.Add(time.Second)); err != nil {
		t.Fatalf("RecordRetrySuccess: %v", err)
	}
	if open, err := s.RetryCircuitOpen(ctx, "workflow:rev-1", now.Add(time.Second)); err != nil || open != nil {
		t.Fatalf("circuit after success = (%+v, %v), want closed", open, err)
	}

	// A failure after the close starts a fresh streak and does not inherit
	// the old open interval.
	state, err := s.RecordRetryFailure(ctx, "workflow:rev-1", "run-b", now.Add(2*time.Second), 3, 15*time.Minute)
	if err != nil {
		t.Fatalf("RecordRetryFailure after success: %v", err)
	}
	if state.ConsecutiveFailures != 1 || state.OpenUntil != nil {
		t.Fatalf("fresh failure state = %+v, want count=1 and closed", state)
	}
}

func TestDelayRunRetryDoesNotConsumeAttempt(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-delay")

	original := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	if scheduled, attempt, err := s.ScheduleRunRetry(ctx, "run-delay", original, "usage_window", "USAGE_LIMIT_BLOCKED", 3); err != nil || !scheduled || attempt != 1 {
		t.Fatalf("ScheduleRunRetry = (%v, %d, %v), want (true, 1, nil)", scheduled, attempt, err)
	}
	armed, err := s.LoadRun(ctx, "run-delay")
	if err != nil || armed.RetryState == nil || armed.RetryState.ScheduledAt == nil {
		t.Fatalf("LoadRun after arm = (%+v, %v), want scheduled_at", armed, err)
	}
	scheduledAt := *armed.RetryState.ScheduledAt
	delayedUntil := original.Add(time.Hour)
	delayed, err := s.DelayRunRetry(ctx, "run-delay", original, delayedUntil)
	if err != nil || !delayed {
		t.Fatalf("DelayRunRetry = (%v, %v), want (true, nil)", delayed, err)
	}
	run, err := s.LoadRun(ctx, "run-delay")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.RetryState == nil || run.RetryState.Attempts != 1 || run.RetryState.RetryAfter == nil || !run.RetryState.RetryAfter.Equal(delayedUntil) {
		t.Fatalf("retry state after delay = %+v, want attempt 1 at %v", run.RetryState, delayedUntil)
	}
	if run.RetryState.ScheduledAt == nil || !run.RetryState.ScheduledAt.Equal(scheduledAt) {
		t.Fatalf("scheduled_at after delay = %v, want preserved %v", run.RetryState.ScheduledAt, scheduledAt)
	}
}
