package store

import (
	"slices"
	"strings"
	"time"
)

// UsageWindowPark is what a run record says about a run waiting on a provider
// usage window: whether it is parked on one, and the earliest instant that
// window lets it make progress again. Read from the record alone — the facts
// travel with the run, never reconstructed from the order of its events.
//
// Every reader of a usage park takes this one reading: the sweeper's due and
// probe passes, the pull-request pause notice, the owner's notification, the
// operator's alert (through retrypolicy.UsageWindowNotBefore, which bounds it
// by the instant the arming chose and the run's own max_wait horizon), and the
// gate reconciler. Two readings of one park disagreed in
// production: the notice promised a resume on the provider's date while the
// probe pass relaunched the run every hour (#3127).
type UsageWindowPark struct {
	// Parked reports a resumable run whose failure was a usage window and
	// whose future a platform continuation still owns: a retry armed for the
	// window, or an arming not yet settled — the engine has written
	// failed_resumable on USAGE_LIMIT_BLOCKED and the runner has not yet
	// armed (or the queue still holds the message). Such a run is not dead.
	// A continuation settled final (the attempt budget spent, the retry
	// abandoned, a DLQ park) ends the park: nothing will wake the run.
	Parked bool
	// ReopensAt is the earliest instant the window the record names lets a
	// parked run make progress: the earlier of the wall the park waits on
	// (RunRetryState.Wall) and the reopening of a credential the launch
	// passed over (Run.SkippedCredReopensAt) — the resume re-resolves the
	// chain and lands on it.
	//
	// It is the RECORD's window, not yet the instant a re-launcher waits
	// for: Wall is the wait's dedup identity, and a park that brought no
	// dated news keeps the wall the row already holds
	// (pkg/runner.usageParkWall) — one that can belong to another credential
	// or to a window the run already got past. The reading every re-launcher
	// takes (retrypolicy.UsageWindowNotBefore) bounds it by the instant the
	// arming itself chose.
	//
	// Zero while the arming has not settled (nothing armed yet: the wall the
	// record holds is a previous park's), and when the record names neither a
	// wall nor a skipped credential (a row armed before the wall was
	// persisted): the window is unreadable and the automatic cadence stays
	// the one the arming chose.
	ReopensAt time.Time
	// Credential names the credential window the park waits on: the
	// credentials the attempt that met the wall sealed, as its paid arming
	// copied them (RunRetryState.WallCredentials), sorted, deduplicated and
	// joined. Runs parked on the same credentials wait on the same provider
	// window, so the sweeper probes that window with ONE of them and
	// releases the others on its answer (pkg/server.sweepCredentialProbes).
	// Read from the retry state, never from Run.CredFingerprints, which
	// every resume re-stamps: the canary of a window must not leave it.
	//
	// The record does not say WHICH sealed credential refused, so the key is
	// the whole set: two runs share a window when they sealed the same
	// credentials, and a probe's answer is a call served on ANY of them
	// (RunRetryState.ProbeAnsweredAt). Two runs of one set walled on
	// different members therefore answer for each other: one released on
	// the other member's answer meets its own wall again and is re-armed
	// free on its announced instant — a pod, counted on the window's probe
	// budget. A run that sealed a second credential beside the walled one
	// waits in a window of its own: one more canary. Empty when the record
	// names no credential (ambient credentials, a row armed before the
	// field): no probe speaks for such a run, and it waits for its window.
	Credential string
}

// UsageWindowPark reads the run record's usage park. Nil-safe.
func (r *Run) UsageWindowPark() UsageWindowPark {
	if r == nil || r.Status != RunStatusFailedResumable || r.FailureCode == FailureDLQParked {
		return UsageWindowPark{}
	}
	rs := r.RetryState
	armed := rs != nil && rs.RetryAfter != nil && rs.Reason == RetryReasonUsageWindow
	settling := r.FailureCode == FailureUsageLimitBlocked && r.ContinuationState != ContinuationFinal
	if !armed && !settling {
		return UsageWindowPark{}
	}
	park := UsageWindowPark{Parked: true, Credential: rs.CredentialWindow()}
	if armed {
		park.ReopensAt = usageWindowReopensAt(rs, r.SkippedCredReopensAt)
	}
	return park
}

// UsageWindowCredential is the credential-window key of a sealed credential
// set: sorted and deduplicated, so the order a resolution stamped the set in
// is no fact about the window. Empty for an empty set.
func UsageWindowCredential(fingerprints []string) string {
	set := make([]string, 0, len(fingerprints))
	for _, fp := range fingerprints {
		if fp = strings.TrimSpace(fp); fp != "" {
			set = append(set, fp)
		}
	}
	slices.Sort(set)
	return strings.Join(slices.Compact(set), ",")
}

// usageWindowReopensAt is the earliest of the instants an ARMED record names
// for the window to reopen, zero when it names none.
func usageWindowReopensAt(rs *RunRetryState, skipped *time.Time) time.Time {
	var earliest time.Time
	take := func(t *time.Time) {
		if t != nil && !t.IsZero() && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t.UTC()
		}
	}
	take(rs.Wall)
	take(skipped)
	return earliest
}
