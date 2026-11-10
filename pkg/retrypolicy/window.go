package retrypolicy

import (
	"math/rand/v2"
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

// UsageWindowComeBack is how long after a usage window's reset a parked run
// comes back: just after it, never exactly on it — a resume a second early,
// or against a provider clock a little behind ours, meets the wall again.
// Every wake armed or deferred on a usage window carries it (the runner's
// arming, the sweeper's window hold).
const UsageWindowComeBack = time.Minute

// UsageWindowNotBefore is THE instant before which no automatic re-launcher
// resumes a run parked on a provider usage window, and whether that instant
// still holds the run at now (#3127). Every automatic path reads it — the
// sweeper's due pass and its pull-forward probe, and through
// UsageParkResumesAt the pull-request pause notice, the owner's notification
// and the operator's alert — so none of them can promise one instant while
// another relaunches before it. The one relaunch before it is the probe of
// the run's CREDENTIAL window (pkg/server's credential probe): one run per
// credential per floor asks the provider whether the wall fell early, and the
// notice names that cadence beside the date.
//
// It is the window the record names (store.Run.UsageWindowPark: the wall the
// park waits on, or a skipped credential's earlier reopening), never past the
// instant the arming itself chose to come back (armedFor), and bounded by the
// run's own max_wait measured from its last paid park
// (RunRetryState.ScheduledAt) — the operator's "wake no later than this and
// check again", which a window further out than the horizon must not
// silently replace. A record that names no window answers (zero, false): the
// automatic cadence stays the one the arming chose, so an unreadable window
// never parks a run indefinitely.
//
// The arming's bound is what makes the wall readable here. The wall is the
// wait's dedup IDENTITY, and a park that brought no dated news keeps the one
// the row already holds (pkg/runner.usageParkWall): a weekly forfait the
// launch passed over while the run now waits on a team key's five hours, or
// a window the run already got past before a blind refusal. Read alone it
// held such a run days past the instant its own arming named — fourteen
// reviews once slept four days that way. The arming's instant is the fact of
// THIS park, so a kept wall never outlives it; what the window still holds is
// a run due EARLIER than its arming chose — a probe's pull whose resume aged
// out.
//
// A run released on its credential window's answer (RunRetryState.Released)
// is not held at all: the window's probe got past the wall, which is the
// news the date was waiting for. Its resume can come long after the release —
// behind the shared circuit, or a transient refusal re-armed on the ordinary
// backoff — and holding it to the date then would undo the answer.
func UsageWindowNotBefore(r *store.Run, now time.Time) (time.Time, bool) {
	park := r.UsageWindowPark()
	if !park.Parked || park.ReopensAt.IsZero() {
		return time.Time{}, false
	}
	if r.RetryState.Released(now) {
		return time.Time{}, false
	}
	notBefore := park.ReopensAt
	if armed := armedFor(r.RetryState, now); armed.Before(notBefore) {
		notBefore = armed
	}
	if r.RetryState != nil && r.RetryState.ScheduledAt != nil {
		if horizon := r.RetryState.ScheduledAt.UTC().Add(ForRun(r).MaxWaitDuration()); horizon.Before(notBefore) {
			notBefore = horizon
		}
	}
	return notBefore, notBefore.After(now)
}

// armedFor is the instant the arming of an armed usage park chose to come
// back: the one a probe's pull set aside while it is still ahead
// (RunRetryState.AnnouncedProbeFrom — the pull moved retry_after to its own
// instant), the armed retry otherwise. Called on an armed row only
// (UsageWindowPark names a window only once the retry is armed).
func armedFor(rs *store.RunRetryState, now time.Time) time.Time {
	if announced := rs.AnnouncedProbeFrom(now); announced != nil {
		return announced.UTC()
	}
	return rs.RetryAfter.UTC()
}

// UsageParkResumesAt is the instant a run parked on a usage window is
// announced to resume: the armed retry, or — when the window every automatic
// re-launcher waits for (UsageWindowNotBefore) holds the run past it, a pull
// whose resume aged out — the come-back past that window, the instant the due
// pass defers the run to (UsageWindowWake, before its jitter). Zero when no
// retry is armed. The pull-request pause notice, the owner's notification and
// the operator's alert all say THIS instant, so one park is never told at two
// different times — and the due pass, which holds a run to the same window,
// keeps it.
func UsageParkResumesAt(r *store.Run, now time.Time) time.Time {
	if r == nil || r.RetryState == nil || r.RetryState.RetryAfter == nil {
		return time.Time{}
	}
	at := r.RetryState.RetryAfter.UTC()
	if notBefore, waits := UsageWindowNotBefore(r, now); waits && notBefore.After(at) {
		at = notBefore.Add(UsageWindowComeBack).UTC()
	}
	return at
}

// UsageWindowWake is when a run held to its usage window (UsageWindowNotBefore
// = notBefore) comes back: the come-back pad past the window, spread by the
// run's own jitter the way its arming spread it — runs parked on one wall
// that all resumed on the instant it fell could exhaust the fresh window at
// once.
func UsageWindowWake(r *store.Run, notBefore time.Time) time.Time {
	at := notBefore.Add(UsageWindowComeBack)
	if j := ForRun(r).JitterDuration(); j > 0 {
		at = at.Add(rand.N(j))
	}
	return at.UTC()
}
