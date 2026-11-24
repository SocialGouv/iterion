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
		wall, skipped, armed := tc.wall, tc.skipped, late.Add(time.Minute)
		r := &Run{Status: RunStatusFailedResumable, FailureCode: FailureUsageLimitBlocked,
			RetryState:           &RunRetryState{RetryAfter: &armed, Reason: RetryReasonUsageWindow, Wall: &wall},
			SkippedCredReopensAt: &skipped}
		if got := r.UsageWindowPark().ReopensAt; !got.Equal(early) {
			t.Errorf("%s: ReopensAt = %s, want the earlier %s", tc.name, got, early)
		}
	}
	// Nothing named: the window is unreadable.
	if got := (&Run{Status: RunStatusFailedResumable, FailureCode: FailureUsageLimitBlocked}).UsageWindowPark().ReopensAt; !got.IsZero() {
		t.Errorf("ReopensAt = %s on a record naming no window, want zero", got)
	}
	// An arming not settled yet: the wall the row holds is a previous park's,
	// so the window it names is not this one.
	wall := late
	settling := &Run{Status: RunStatusFailedResumable, FailureCode: FailureUsageLimitBlocked,
		RetryState: &RunRetryState{Wall: &wall, Reason: RetryReasonUsageWindow}}
	if park := settling.UsageWindowPark(); !park.Parked || !park.ReopensAt.IsZero() {
		t.Errorf("settling park = %+v, want parked with no window until the arming lands", park)
	}
}

// The credential window is the sealed SET: the order a resolution stamped it
// in, a duplicate or a blank entry is no fact about which window the run
// waits on, and a run that names no credential belongs to no window.
func TestUsageWindowPark_CredentialIsTheSealedSetInAnyOrder(t *testing.T) {
	park := func(fps ...string) string {
		return (&Run{Status: RunStatusFailedResumable, FailureCode: FailureUsageLimitBlocked,
			RetryState: &RunRetryState{WallCredentials: fps}}).UsageWindowPark().Credential
	}
	if a, b := park("fp-zai", "fp-codex"), park("fp-codex", " fp-zai", "fp-codex", ""); a != b || a != "fp-codex,fp-zai" {
		t.Fatalf("Credential = %q and %q, want both %q", a, b, "fp-codex,fp-zai")
	}
	if got := park("fp-zai"); got == park("fp-zai", "fp-codex") {
		t.Fatalf("a run that sealed a second credential shares the window key %q of a run that sealed one", got)
	}
	if got := park(); got != "" {
		t.Fatalf("Credential = %q on a record naming no credential, want empty", got)
	}
	if got := (&Run{Status: RunStatusQueued, RetryState: &RunRetryState{WallCredentials: []string{"fp-zai"}}}).UsageWindowPark().Credential; got != "" {
		t.Fatalf("Credential = %q on a run that is not parked, want empty", got)
	}
}

// The window is the credentials the wall was met on, as the paid arming
// copied them — never the run's current stamp, which every resume re-writes:
// a probe's canary whose re-resolution sealed another set (the next pool
// slot, an empty resolution) stays in the window it was pulled for.
func TestUsageWindowPark_CredentialIsTheWallsNotTheResumesStamp(t *testing.T) {
	for _, restamp := range [][]string{nil, {"fp-zai-slot-2"}} {
		r := &Run{Status: RunStatusFailedResumable, FailureCode: FailureUsageLimitBlocked,
			CredFingerprints: restamp,
			RetryState:       &RunRetryState{WallCredentials: []string{"fp-zai"}}}
		if got := r.UsageWindowPark().Credential; got != "fp-zai" {
			t.Errorf("re-stamped %v: Credential = %q, want fp-zai — the window the wall was met on", restamp, got)
		}
	}
}
