package store

import (
	"testing"
	"time"
)

func TestUsageWindowPark_ParkedOnlyWhileAContinuationOwnsIt(t *testing.T) {
	at := time.Date(2026, 11, 14, 17, 24, 0, 0, time.UTC)
	armed := func(reason string) *RunRetryState { return &RunRetryState{RetryAfter: &at, Wall: &at, Reason: reason} }
	cases := []struct {
		name string
		run  Run
		want bool
	}{
		{"armed on a usage window", Run{Status: RunStatusFailedResumable, RetryState: armed(RetryReasonUsageWindow)}, true},
		{"failed on a usage window, arming still settling", Run{Status: RunStatusFailedResumable, FailureCode: FailureUsageLimitBlocked}, true},
		{"usage window, redelivery pending", Run{Status: RunStatusFailedResumable, FailureCode: FailureUsageLimitBlocked, ContinuationState: ContinuationRedeliveryPending}, true},
		{"usage window settled final", Run{Status: RunStatusFailedResumable, FailureCode: FailureUsageLimitBlocked, ContinuationState: ContinuationFinal}, false},
		{"a DLQ park whatever its retry state says", Run{Status: RunStatusFailedResumable, FailureCode: FailureDLQParked, RetryState: armed(RetryReasonUsageWindow)}, false},
		{"a credential-slot wait is not a usage window", Run{Status: RunStatusFailedResumable, RetryState: armed(RetryReasonCredSlot)}, false},
		{"resumed: queued", Run{Status: RunStatusQueued, RetryState: armed(RetryReasonUsageWindow)}, false},
	}
	for _, tc := range cases {
		if got := tc.run.UsageWindowPark().Parked; got != tc.want {
			t.Errorf("%s: Parked = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The earliest instant the record names, whichever field names it: the order
// of the two facts on the record is no fact about the window.
func TestUsageWindowPark_ReopensAtTheEarlierOfTheWallAndTheSkippedCredential(t *testing.T) {
	early := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	late := time.Date(2026, 11, 14, 17, 24, 0, 0, time.UTC)
	for _, tc := range []struct {
		name          string
		wall, skipped time.Time
	}{
		{"skipped credential reopens first", late, early},
		{"wall falls first", early, late},
	} {
		wall, skipped := tc.wall, tc.skipped
		r := &Run{Status: RunStatusFailedResumable, FailureCode: FailureUsageLimitBlocked,
			RetryState: &RunRetryState{Wall: &wall}, SkippedCredReopensAt: &skipped}
		if got := r.UsageWindowPark().ReopensAt; !got.Equal(early) {
			t.Errorf("%s: ReopensAt = %s, want the earlier %s", tc.name, got, early)
		}
	}
	// Nothing named: the window is unreadable.
	if got := (&Run{Status: RunStatusFailedResumable, FailureCode: FailureUsageLimitBlocked}).UsageWindowPark().ReopensAt; !got.IsZero() {
		t.Errorf("ReopensAt = %s on a record naming no window, want zero", got)
	}
}
