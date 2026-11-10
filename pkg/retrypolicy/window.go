package retrypolicy

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ForRun resolves the retry policy a run was launched with. The policy was
// resolved across its layers at LAUNCH and snapshotted on the run document,
// so a reader only re-applies the platform ceiling; a run predating the
// snapshot (nil) gets the package defaults under that same ceiling.
func ForRun(r *store.Run) Policy {
	var pol Policy
	if r != nil && r.RetryPolicy != nil {
		pol = Policy{
			UsageWindow: r.RetryPolicy.UsageWindow,
			MaxAttempts: r.RetryPolicy.MaxAttempts,
			MaxWait:     r.RetryPolicy.MaxWait,
			Jitter:      r.RetryPolicy.Jitter,
		}
	}
	return Clamp(Normalize(pol), CeilingFromEnv(), nil)
}

// UsageWindowNotBefore is THE instant before which no automatic re-launcher
// resumes a run parked on a provider usage window, and whether that instant
// still holds the run at now (#3127). Every automatic path reads it — the
// sweeper's due pass and its pull-forward probe, the pull-request pause
// notice, the local auto-resume loop — so none of them can promise one
// instant while another relaunches before it.
//
// It is the window the record names (store.Run.UsageWindowPark: the wall the
// park waits on, or a skipped credential's earlier reopening), bounded by
// the run's own max_wait measured from its last paid park
// (RunRetryState.ScheduledAt) — the operator's "wake no later than this and
// check again", which a window further out than the horizon must not
// silently replace. A record that names no window answers (zero, false): the
// automatic cadence stays the one the arming chose, so an unreadable window
// never parks a run indefinitely.
func UsageWindowNotBefore(r *store.Run, now time.Time) (time.Time, bool) {
	park := r.UsageWindowPark()
	if !park.Parked || park.ReopensAt.IsZero() {
		return time.Time{}, false
	}
	notBefore := park.ReopensAt
	if r.RetryState != nil && r.RetryState.ScheduledAt != nil {
		if horizon := r.RetryState.ScheduledAt.UTC().Add(ForRun(r).MaxWaitDuration()); horizon.Before(notBefore) {
			notBefore = horizon
		}
	}
	return notBefore, notBefore.After(now)
}
