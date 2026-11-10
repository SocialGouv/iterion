package store

import "time"

// UsageWindowPark is what a run record says about a run waiting on a provider
// usage window: whether it is parked on one, and the earliest instant that
// window lets it make progress again. Read from the record alone — the facts
// travel with the run, never reconstructed from the order of its events.
//
// Every reader of a usage park takes this one reading: the sweeper's due and
// probe passes, the pull-request pause notice, the gate reconciler, the local
// auto-resume loop (through retrypolicy.UsageWindowNotBefore, which adds the
// run's own max_wait horizon). Two readings of one park disagreed in
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
	// ReopensAt is the earliest instant the window lets a parked run make
	// progress: the earlier of the wall the park waits on (RunRetryState.Wall)
	// and the reopening of a credential the launch passed over
	// (Run.SkippedCredReopensAt) — the resume re-resolves the chain and lands
	// on it. Zero when the record names neither (a row armed before the wall
	// was persisted names none): the window is unreadable and the automatic
	// cadence stays the one the arming chose.
	ReopensAt time.Time
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
	return UsageWindowPark{Parked: true, ReopensAt: usageWindowReopensAt(rs, r.SkippedCredReopensAt)}
}

// usageWindowReopensAt is the earliest of the instants the record names for
// the window to reopen, zero when it names none.
func usageWindowReopensAt(rs *RunRetryState, skipped *time.Time) time.Time {
	var earliest time.Time
	take := func(t *time.Time) {
		if t != nil && !t.IsZero() && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t.UTC()
		}
	}
	if rs != nil {
		take(rs.Wall)
	}
	take(skipped)
	return earliest
}
