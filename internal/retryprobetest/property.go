package retryprobetest

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The guarantee of the pull-forward probe, as predicates over the row.
//
// Three adversarial rounds on this change each found the NEXT unhandled
// interleaving of a retry row whose probe is in flight — who pays for an
// arming, which instant survives it, when the marker expires, what is
// refunded, what is counted — and each fix was a regression of the previous
// one's. A state machine reviewed by reading does not converge, so what
// follows is the guarantee itself, asserted after EVERY step of random but
// reproducible sequences driven over the real functions:
//
//	I1  a probe-driven resume consumes no retry attempt and no monthly quota
//	    unit (its launch-rate token is the same rollback, pinned from the
//	    other side by the admission steps the server driver takes with a
//	    bucket of one).
//	I2  the announced instant is never replaced by a blind guess — only by a
//	    fresher DATED instant, or by a credential's own reopening when the
//	    refusal that named it was itself dated.
//	I3  a park that followed real work (tokens spent) is charged, counted,
//	    fed to the shared circuit and consumes the probe marker — exactly
//	    once; and every arming of the row reaches the operator's counter once.
//	I4  a probe-marked row is never retired before its announced instant, and
//	    no probe ever pushes attempts past the budget.
//	I5  one pause notice per provider wall — and not none: a notice offered on
//	    an armed row whose probe ledger is closed is posted. The same for the
//	    run owner's own notification: at most one per wall, and the park that
//	    opens a wall delivers it.
//	I6  a marker nobody is paying for is read by NOBODY: an instant already
//	    passed, or a pull older than store.ProbeMarkerTTL, and every payment
//	    step on such a row behaves exactly as it would with no marker at all.
//	I7  the probe ledger belongs to the WALL: no arming that re-states the
//	    wall the row is on clears it, no arming that MOVES the wall inherits
//	    it, and the pulls one wall spends never pass the probe budget.
//
// Each predicate names the violating step in its message; the driver replays
// the failing sequence's prefixes to report the shortest one that violates.

// StepKind names one event a sequence may contain. A driver declares the
// kinds it can drive REALLY — the park kinds need pkg/runner's arming, the
// sweep kinds need pkg/server's passes — and Generate draws from that set.
type StepKind string

const (
	// StepParkDated: the run met the window again and the refusal named a
	// reset instant.
	StepParkDated StepKind = "park_dated"
	// StepParkBlind: the same, with a refusal that named no instant — the
	// case whose decision is a GUESS.
	StepParkBlind StepKind = "park_blind"
	// StepParkRedated: the refusal names a window DURATION ("reached for 5
	// hour"), so the instant it resolves to is recomputed from the moment it
	// is read and every reading of ONE provider window answers a different
	// one. The third production refusal shape, and the one no per-WALL
	// invariant can see: a defect that invents a new wall per park satisfies
	// "one notification per wall" and "budget probes per wall" while the
	// owner's phone and the probe pass run at the sweeper's cadence.
	StepParkRedated StepKind = "park_redated"
	// StepParkAfterWorkDated / StepParkAfterWorkBlind: the window had
	// genuinely reopened, the execution spent tokens, and THEN met a wall.
	StepParkAfterWorkDated StepKind = "park_after_work_dated"
	StepParkAfterWorkBlind StepKind = "park_after_work_blind"
	// StepSkippedSooner / StepSkippedLater: a credential the run skipped
	// reopens before / after the wall it waits on.
	StepSkippedSooner StepKind = "skipped_sooner"
	StepSkippedLater  StepKind = "skipped_later"
	// StepSkippedCleared: the reopening is no longer known.
	StepSkippedCleared StepKind = "skipped_cleared"

	// StepProbePull is the store CAS the probe pass performs, taken
	// directly — the pass's own scan and metric are what StepProbeSweep
	// drives.
	StepProbePull StepKind = "probe_pull"
	// StepResumeClear is what a resume does to the row (ClearRunRetry),
	// taken directly for the drivers that have no sweeper.
	StepResumeClear StepKind = "resume_clear"
	// StepAgeMarker is the passage of time that ends the wall a probe is
	// holding: the marker stays on the row, its instant is now behind us.
	StepAgeMarker StepKind = "age_marker"
	// StepAgePull is the other passage of time the row records: the PULL
	// recedes while the instant it set aside is still ahead — the row a
	// probe-driven resume that never parked leaves behind, and the one state
	// where the wall and the payment answers legitimately disagree. It also
	// re-opens the probe pace, which is what lets a sequence pull the same
	// wait forward twice.
	StepAgePull StepKind = "age_pull"
	// StepDue is the passage of time that brings an armed instant: the row is
	// due on its own account, as the due scan sees it. Without it no sequence
	// could ever resume a row the previous step armed for the future — and the
	// treadmill of parks that follows an arming, the failure mode the ticket
	// is about, would not be modelled at all.
	StepDue StepKind = "due"
	// StepProbeSweep, StepDueSweep*, StepNotice and StepPaidPark are the
	// server's real passes.
	StepProbeSweep StepKind = "probe_sweep"
	// StepDueSweep: the due pass with a resume that lands.
	StepDueSweep StepKind = "due_sweep"
	// StepDueSweepResumeFails: a transient publish error — reArmRetry.
	StepDueSweepResumeFails StepKind = "due_sweep_resume_fails"
	// StepDueSweepPoolRemap: a permanent refusal — abandonRetry.
	StepDueSweepPoolRemap StepKind = "due_sweep_pool_remap"
	// StepNotice fires the pull request's pause notice on the row.
	StepNotice StepKind = "notice"
	// StepPaidPark is the arming the runner's PAID path performs, as the
	// row sees it: one attempt, a new wall, the ledger closed. Who pays is
	// decided in pkg/runner and asserted by the runner driver; here it is
	// the precondition that opens a new wall.
	StepPaidPark StepKind = "paid_park"
	// StepUserNotify offers the run's current outcome to the user-notification
	// dispatcher, as the eventbus and the reconciliation sweep both do. It is
	// the step the ticket's headline is about: the owner of a parked run hears
	// about the WALL, once.
	StepUserNotify StepKind = "user_notify"
)

// Step is one generated event, plus the facts the invariants read from it.
// The driver fills Spend and Skipped from the world it drives.
type Step struct {
	Kind StepKind
	// At is the instant the refusal named (zero when it named none), or the
	// instant a paid park arms.
	At time.Time
	// Spend says the execution that parked got an answer the provider
	// charged for.
	Spend bool
	// Skipped is the skipped credential's reopening the row carried when
	// this step ran.
	Skipped time.Time
}

// Dated reports that this step fed the decision an instant of its own.
func (s Step) Dated() bool { return !s.At.IsZero() }

// IsPark reports a step that goes through the runner's arming.
func (s Step) IsPark() bool {
	switch s.Kind {
	case StepParkDated, StepParkBlind, StepParkRedated, StepParkAfterWorkDated, StepParkAfterWorkBlind:
		return true
	}
	return false
}

// IsResume reports a step whose success enqueues a resume (and so passes the
// metered admission gate).
func (s Step) IsResume() bool { return s.Kind == StepDueSweep }

func (s Step) String() string {
	out := string(s.Kind)
	if s.Dated() {
		out += "@" + s.At.UTC().Format(time.RFC3339)
	}
	if s.Spend {
		out += "+spent"
	}
	return out
}

// wallOffsets is the instant palette: inside the probe floor, just outside
// it, and the multi-day resets the ticket measured.
func wallOffsets() []time.Duration {
	return []time.Duration{30 * time.Minute, 95 * time.Minute, 4 * time.Hour, 4 * 24 * time.Hour, 7 * 24 * time.Hour}
}

// Generate draws a reproducible sequence of 1..40 steps from kinds.
//
// Seeded on purpose: a failing sequence is replayable from its seed alone,
// and the driver shrinks it by replaying its prefixes.
func Generate(seed uint64, kinds []StepKind, base time.Time) []Step {
	r := rand.New(rand.NewPCG(seed, 0x2247))
	n := 1 + r.IntN(40)
	offsets := wallOffsets()
	out := make([]Step, 0, n)
	for i := 0; i < n; i++ {
		k := kinds[r.IntN(len(kinds))]
		st := Step{Kind: k}
		switch k {
		case StepParkDated, StepParkAfterWorkDated, StepPaidPark:
			st.At = base.Add(offsets[r.IntN(len(offsets))])
		}
		switch k {
		case StepParkAfterWorkDated, StepParkAfterWorkBlind:
			st.Spend = true
		case StepPaidPark:
			// The paid park IS the charged one: excluded from I1 for the
			// same reason a park that spent tokens is.
			st.Spend = true
		}
		out = append(out, st)
	}
	return out
}

// Snapshot is the world as the invariants read it, before and after a step.
// A driver fills the counters it owns; the ones it does not stay 0, and the
// predicates that read them then compare 0 with 0.
type Snapshot struct {
	Row          *store.RunRetryState
	Status       store.RunStatus
	Continuation store.ContinuationState

	// Metered is the org's monthly run-quota units charged so far.
	Metered int
	// Resumes is the resumes actually enqueued.
	Resumes int
	// Blocked is iterion_runs_usage_window_blocked_total.
	Blocked int
	// Scheduled is iterion_runs_retry_scheduled_total.
	Scheduled int
	// Armings is the writes that armed this row, whoever paid: the paid
	// arming and the free re-arm. What Scheduled is compared against.
	Armings int
	// Circuit is the shared retry circuit's recorded failures.
	Circuit int
	// Notices is the pause notices posted on the pull request.
	Notices int
	// UserNotices is the notifications delivered to the run's OWNER
	// (pkg/usernotify). A driver that does not wire the dispatcher leaves it
	// at 0, and I5's user half then compares 0 with 0.
	UserNotices int
	// Abandons is the settlements that retired the retry.
	Abandons int
}

// Attempts is the row's lifetime attempt count (0 when nothing is armed).
func (s Snapshot) Attempts() int {
	if s.Row == nil {
		return 0
	}
	return s.Row.Attempts
}

// Held is the furthest instant the row is still holding: the armed one, or
// the one a probe set aside. Zero when it holds neither.
func (s Snapshot) Held() time.Time {
	if s.Row == nil {
		return time.Time{}
	}
	out := time.Time{}
	if s.Row.RetryAfter != nil {
		out = s.Row.RetryAfter.UTC()
	}
	if s.Row.ProbeFrom != nil && s.Row.ProbeFrom.After(out) {
		out = s.Row.ProbeFrom.UTC()
	}
	return out
}

// AnnouncedMarker reports a row holding the provider's own dated word: a
// marker whose instant is still ahead, whatever the age of the pull. The WALL
// question (store.RunRetryState.AnnouncedProbeFrom), modelled from the raw
// fields on purpose — a checker that asked the accessor would agree with a
// broken one instead of catching it.
func (s Snapshot) AnnouncedMarker(now time.Time) bool {
	return s.Row != nil && s.Row.ProbeFrom != nil && s.Row.ProbeFrom.After(now)
}

// LiveMarker reports a probe in flight: the PAYMENT question
// (store.RunRetryState.ProbeInFlight) — an instant still ahead AND a pull
// recent enough that the resume it started can still be the execution talking.
func (s Snapshot) LiveMarker(now time.Time) bool {
	if !s.AnnouncedMarker(now) {
		return false
	}
	return s.Row.LastProbeAt != nil && s.Row.LastProbeAt.After(now.Add(-store.ProbeMarkerTTL))
}

// StaleMarker reports a marker whose PULL is too old to be paying for anything
// while its instant is still ahead: the row a probe-driven resume that never
// parked leaves behind. Every payment reader must treat it as no marker at
// all, and every wall reader must still honour its instant.
func (s Snapshot) StaleMarker(now time.Time) bool {
	return s.AnnouncedMarker(now) && !s.LiveMarker(now)
}

// ExpiredMarker reports a marker left over from a wait that has ENDED — the
// shape every reader, wall or payment, must treat as no marker at all.
func (s Snapshot) ExpiredMarker(now time.Time) bool {
	return s.Row != nil && s.Row.ProbeFrom != nil && !s.Row.ProbeFrom.After(now)
}

// Retired reports a row with nothing armed and its continuation closed.
func (s Snapshot) Retired() bool {
	return s.Continuation == store.ContinuationFinal
}

// Ledger is what the SEQUENCE knows and the code under test must honour: the
// wall the provider announced, the walls a paid arming opened, the launch
// tokens an ordinary resume legitimately spent.
type Ledger struct {
	// MaxAttempts is the row's attempt budget.
	MaxAttempts int
	// Wall is the latest instant the provider named to this row. It moves
	// only on fresh DATED news (earlier or later: good news from the
	// provider is not a probe's to override) and is dropped when the run
	// leaves failed_resumable, where no automatic wait is owed any more.
	Wall time.Time
	// WallsOpened counts the paid armings that started a wait the pull
	// request has never been told about: I5's budget for notices.
	WallsOpened int
	// TokensSpent counts the ordinary (unprobed) resumes that legitimately
	// took the minute's launch-rate token.
	TokensSpent int
	// ProbeBudget is the pulls ONE wall may spend (the deployment's
	// ITERION_RETRY_PROBE_BUDGET). Zero means the driver does not drive the
	// probe pass, and I7's bound is then not its business.
	ProbeBudget int
	// ProbeFloor is the deployment's ITERION_RETRY_PROBE_FLOOR: the shortest
	// interval between two pulls of one ROW. Zero means the driver does not
	// drive the probe pass, and I8's cadence half is then not its business.
	ProbeFloor time.Duration
	// Steps is the sequence so far, for the failure message.
	Steps []Step

	// notified counts, per provider wall (keyed on its second), the
	// notifications the run's owner has had about it.
	notified map[int64]int
}

// notifiedWall is what the owner has already heard about this wall.
func (l *Ledger) notifiedWall(wall time.Time) int { return l.notified[wall.Unix()] }

// armedHere reports a step that wrote an arming: attempts moved (a charged
// park) or the marker was consumed while the row stayed armed (a free
// re-arm).
func armedHere(before, after Snapshot) bool {
	if after.Attempts() > before.Attempts() {
		return true
	}
	return before.Row != nil && before.Row.ProbeFrom != nil && after.Row != nil && after.Row.ProbeFrom == nil
}

// wallMoved reports that an arming MOVED the wall: the row carried one and the
// arming wrote a different one. The legacy shape, which carried none at all,
// is not a move — the budget on such a row is the budget of the wait it is
// already on, and probe_count predates the wall field.
func wallMoved(before, after *store.RunRetryState) bool {
	if after == nil || after.Wall == nil || before == nil || before.Wall == nil {
		return false
	}
	return !before.Wall.Equal(*after.Wall)
}

// ParkedWall is the provider wall this run is PARKED on, zero when it is not
// parked on one — the fact every notification, alert and pull-request notice
// about a usage park keys on (store.UsageParkWall), modelled here from the raw
// fields on purpose: a checker that asked the production accessor would agree
// with a broken one instead of catching it.
func (s Snapshot) ParkedWall() time.Time {
	if s.Status != store.RunStatusFailedResumable || s.Row == nil {
		return time.Time{}
	}
	if s.Row.RetryAfter == nil || s.Row.Wall == nil || s.Row.Reason != store.RetryReasonUsageWindow {
		return time.Time{}
	}
	return s.Row.Wall.UTC()
}

// Observe folds one executed step into the ledger and returns the invariants
// it violated, each naming the step and what the row looks like.
func (l *Ledger) Observe(st Step, before, after Snapshot, now time.Time) []string {
	l.Steps = append(l.Steps, st)
	var bad []string
	fail := func(id, format string, args ...any) {
		bad = append(bad, fmt.Sprintf("%s violated by step %d (%s): %s\n  row before: %s\n  row after:  %s\n  sequence:   %s",
			id, len(l.Steps), st, fmt.Sprintf(format, args...),
			describe(before.Row), describe(after.Row), describeSteps(l.Steps)))
	}

	liveBefore := before.LiveMarker(now)
	announcedBefore := before.AnnouncedMarker(now)
	staleBefore := before.StaleMarker(now)
	expiredBefore := before.ExpiredMarker(now)
	// A step that ages the marker IS the wall passing: there is no future
	// instant left for anyone to hold, and every later step on that row is
	// I6's business. A row made DUE says the same thing about the instant it
	// was armed for: it has arrived.
	if (st.Kind == StepAgeMarker && announcedBefore) || st.Kind == StepDue {
		l.Wall = time.Time{}
	}
	resumable := before.Status == store.RunStatusFailedResumable
	room := before.Attempts() < l.MaxAttempts

	// The floor this step must leave the row holding: the announced wall,
	// unless the step brought fresher DATED news (which wins, even earlier).
	//
	// Keyed on the ledger's wall alone, NOT on whether the row is still
	// holding it: a clause that stopped looking the moment a step moved the
	// row off the wall left every later step of that sequence unchecked.
	// What a step is ALLOWED to do lowers the floor for THAT step, and the
	// bookkeeping below then lowers the ledger's wall to whatever the row
	// legitimately ended up on — so the next step is held to that, and not to
	// nothing.
	floor := time.Time{}
	if l.Wall.After(now) {
		floor = l.Wall
	}
	if st.Dated() {
		if !floor.IsZero() && st.At.Before(floor) {
			floor = st.At
		}
		l.Wall = st.At
	}
	// A skipped credential's own reopening may move a wall the row GUESSED,
	// never one the provider announced and the row is still holding aside:
	// the arming that takes the early wake consumes the marker in the same
	// write, so the provider's word would be traded for a speculation about
	// another credential — days before the window it names.
	if !announcedBefore && !floor.IsZero() && !st.Skipped.IsZero() && st.Skipped.Before(floor) {
		floor = st.Skipped
	}

	// I1 — a probe-driven resume or re-park spends nothing. Scoped to a row
	// whose marker is LIVE and a step that spent nothing on the provider:
	// that pair IS "this is the probe's own speculative refusal".
	if liveBefore && !st.Spend {
		if d := after.Attempts() - before.Attempts(); d != 0 {
			fail("I1", "a probe-driven step charged %d retry attempt(s): an hourly probe against a four-day reset would exhaust the budget in hours and retire the run days before its wall falls", d)
		}
		if d := after.Metered - before.Metered; d != 0 {
			fail("I1", "a probe-driven step metered %d monthly run-quota unit(s): one parked wait would spend its whole probe budget of the org's month and 402 the tenant's real launches", d)
		}
	}

	// I1b — and what BOUNDS that refund: a resume no probe drove is an
	// ordinary billable launch. A refund that fires on every resume
	// under-counts the tenant's real executions for good, which is the same
	// defect with the sign flipped.
	if st.IsResume() && after.Resumes > before.Resumes && !liveBefore {
		if d := after.Metered - before.Metered; d != 1 {
			fail("I1", "a resume no probe drove metered %d monthly run-quota unit(s), want exactly 1: the refund is the probe's, and a tenant whose real executions stop being counted is billed for nothing", d)
		}
	}

	// I1c — and the third thing a probe must not spend: the minute's
	// launch-rate token. The gate's bucket is internal, but a token a probe
	// took and never gave back DENIES the next launch of the same minute,
	// and the sequence then shows a row that was due, free of any lease, and
	// that nothing resumed. Scoped to "no ordinary resume has taken the
	// minute's token yet", because after one legitimately has, a denial is
	// the bucket doing its job.
	if st.IsResume() && l.TokensSpent == 0 && dueAndUnleased(before, now) && after.Resumes == before.Resumes {
		fail("I1", "a row due at %s with no live lease was not resumed: a probe's resume that keeps the minute's launch token crowds out the pass that resumes runs whose own instant has arrived",
			instant(before.Held()))
	}

	// I2 — the announced instant survives, armed or held aside. Exempt: a
	// row with no live marker that this step legitimately retired (whose
	// own correctness is the sweeper's settlement tests), and a run an
	// operator took out of failed_resumable.
	retiredHere := after.Retired() && !before.Retired()
	if !floor.IsZero() && resumable && (!retiredHere || announcedBefore) {
		// Compared as the DATABASE holds it: the row cannot carry a finer
		// instant than the millisecond BSON stores, so a floor demanded to
		// the nanosecond is a floor no row can ever meet.
		if held := after.Held(); held.Before(asStored(floor)) {
			fail("I2", "the row now holds %s, which is before the instant it was told to wait for (%s): a guess written over the provider's own word deletes the only wall this run was ever given",
				instant(held), instant(floor))
		}
	}

	// I3 — a park that followed real work is an ordinary new wall: charged,
	// counted, fed to the circuit, and the probe ledger closed.
	if st.IsPark() && st.Spend && resumable {
		if room {
			if d := after.Attempts() - before.Attempts(); d != 1 {
				fail("I3", "a park that followed real work charged %d attempt(s), want exactly 1: the window had reopened, this run used it, and the wall it met is new", d)
			}
			if d := after.Blocked - before.Blocked; d != 1 {
				fail("I3", "a park that followed real work counted %d usage-window block(s), want exactly 1", d)
			}
			if d := after.Circuit - before.Circuit; d != 1 {
				fail("I3", "a park that followed real work fed the shared circuit %d time(s), want exactly 1", d)
			}
			if after.Row != nil && after.Row.ProbeFrom != nil {
				fail("I3", "a charged park left the probe MARKER on the row (probe_from=%s): the next park would take the free treatment on a wall this execution already used",
					instantPtr(after.Row.ProbeFrom))
			}
		} else if d := after.Attempts() - before.Attempts(); d != 0 {
			fail("I3", "the attempt budget was spent, yet the arming charged %d more", d)
		}
	}

	// I3b — and every arming of this row reaches the operator's counter
	// exactly once, whoever paid for it: iterion_runs_retry_scheduled_total is
	// the ONE signal that runs are being armed, and a free re-arm that armed
	// the row without moving it makes a run parked for days invisible to it.
	if st.IsPark() && after.Armings > before.Armings {
		armed := after.Armings - before.Armings
		if d := after.Scheduled - before.Scheduled; d != armed {
			fail("I3", "the row was armed %d time(s) and the operator's retry-scheduled counter moved %d: an arming nothing counts is a parked run no dashboard can see", armed, d)
		}
	}

	// I4 — a probe may give a run an earlier chance; never a shorter life.
	// Keyed on the WALL door (a marker whose instant is still ahead, whatever
	// the age of the pull), because that is exactly the read the production
	// guard makes before it settles: the two cannot drift.
	if announcedBefore && l.Wall.After(now) && resumable {
		if retiredHere || after.Abandons > before.Abandons {
			fail("I4", "a probe-marked row was retired while the wall it waits on (%s) is still ahead", instant(l.Wall))
		}
	}
	if after.Attempts() > l.MaxAttempts {
		fail("I4", "attempts = %d, past the budget of %d", after.Attempts(), l.MaxAttempts)
	}

	// I5 — one notice per wall. A probe's re-park says nothing new, and the
	// pull request has no dedup of its own.
	if d := after.Notices - before.Notices; d != 0 {
		if before.Row != nil && before.Row.ProbeCount > 0 {
			fail("I5", "a probe's re-park posted %d notice(s) on the pull request: with the default budget one provider wall would collect a whole budget of byte-identical comments", d)
		}
	}
	if after.Notices > l.WallsOpened {
		fail("I5", "%d notices for %d wall(s) opened", after.Notices, l.WallsOpened)
	}
	// I5b — and the direction a suppression hides in: "at most one" is also
	// satisfied by none. A notice OFFERED on an armed row whose probe ledger
	// is CLOSED is the park that started the wait speaking, and it must be
	// posted — otherwise the pull request holds a required check in flight for
	// the whole wait with no comment.
	if st.Kind == StepNotice && before.Row != nil && before.Row.RetryAfter != nil && before.Row.ProbeCount == 0 {
		if after.Notices == before.Notices {
			fail("I5", "a pause notice was offered on a row armed for %s with a closed probe ledger and nothing was posted: the gating check then sits in flight for the whole wait with no comment",
				instantPtr(before.Row.RetryAfter))
		}
	}

	// I6 — a marker nobody is paying for is no marker. Two shapes answer that
	// way, and the PAYMENT readers (store.RunRetryState.ProbeInFlight) must
	// treat both exactly as they would a bare row — the park is charged, the
	// resume is metered: a marker whose instant has PASSED (the wait is over)
	// and one whose PULL is older than store.ProbeMarkerTTL (the resume it
	// started is long gone, and this park is nobody's probe). The wall door
	// deliberately disagrees about the second: I2 still holds the row to the
	// instant that marker names.
	if (expiredBefore || staleBefore) && resumable {
		dead := "whose instant had PASSED"
		if staleBefore {
			dead = "left over from a pull too old to be paying for anything"
		}
		if st.IsPark() && !st.Spend && room {
			if d := after.Attempts() - before.Attempts(); d != 1 {
				fail("I6", "a park on a row carrying a probe marker %s charged %d attempt(s), want 1: a free arming bought by a dead marker is an attempt nobody pays, and the block it hides is counted by nobody and announced to nobody", dead, d)
			}
		}
		if st.IsResume() && after.Resumes > before.Resumes {
			if d := after.Metered - before.Metered; d != 1 {
				fail("I6", "a resume on a row carrying a probe marker %s metered %d unit(s), want 1: that row is due on its own account and its resume is an ordinary billable launch", dead, d)
			}
		}
	}

	// I7 — the probe BUDGET belongs to the WALL. Two directions, and each is a
	// measured cost: a budget cleared by an arming that re-states the wall
	// prices one provider window at max_attempts × budget (120 pods and 5
	// metered launches at the shipped defaults), and a budget INHERITED by a
	// wall the provider moved leaves a genuinely new window unprobed until the
	// run simply waits it out.
	//
	// The PACE is not the wall's and no arming may clear it — I8.
	if before.Row != nil && after.Row != nil && after.Row.RetryAfter != nil && armedHere(before, after) {
		if wallMoved(before.Row, after.Row) {
			if after.Row.ProbeCount != 0 {
				fail("I7", "an arming moved the wall from %s to %s and kept the previous wall's budget (probe_count=%d): the new window is probed on a budget it never spent",
					instantPtr(before.Row.Wall), instantPtr(after.Row.Wall), after.Row.ProbeCount)
			}
		} else if after.Row.ProbeCount < before.Row.ProbeCount {
			fail("I7", "an arming that left the wall at %s dropped the budget from %d to %d: the same window is probed afresh, which is the max_attempts × budget ceiling this rule exists to remove",
				instantPtr(after.Row.Wall), before.Row.ProbeCount, after.Row.ProbeCount)
		}
	}
	// I8 — the PACE belongs to the ROW, and nothing an arming does may hand a
	// run a faster cadence than one pull per probe floor. Stated per RUN and
	// not per wall on purpose: a defect that invents a new wall on every
	// reading of one refusal satisfies every per-wall bound there is, and this
	// is the clause that sees it. Two halves — the stamp an arming must leave
	// alone, and the cadence the pulls must keep.
	if before.Row != nil && after.Row != nil && armedHere(before, after) &&
		before.Row.LastProbeAt != nil && after.Row.LastProbeAt == nil {
		fail("I8", "an arming dropped last_probe_at (the pull of %s): the probe scan reads an absent stamp as \"never probed\", so this row is eligible again on the sweeper's NEXT pass instead of one floor later — a refusal that re-dates its window on every reading is then pulled once per sweep tick, charged nothing and bounded by nothing",
			instantPtr(before.Row.LastProbeAt))
	}
	if l.ProbeFloor > 0 && before.Row != nil && after.Row != nil &&
		after.Row.ProbeCount > before.Row.ProbeCount && before.Row.LastProbeAt != nil {
		if since := now.Sub(before.Row.LastProbeAt.UTC()); since < l.ProbeFloor {
			fail("I8", "the probe pass pulled this row %s after the previous pull (%s), inside the probe floor of %s: one pod per floor per run is the bound, whatever the provider does to its dates",
				since, instantPtr(before.Row.LastProbeAt), l.ProbeFloor)
		}
	}
	if l.ProbeBudget > 0 && after.Row != nil && after.Row.ProbeCount > l.ProbeBudget {
		fail("I7", "the wall at %s has been pulled forward %d times, past the budget of %d: a forfait nobody resets must stop being probed and simply wait",
			instantPtr(after.Row.Wall), after.Row.ProbeCount, l.ProbeBudget)
	}

	// I9 — only the PROVIDER moves a wall. A park whose refusal named no
	// instant of its own says nothing about where this window ends: a window
	// DURATION resolves a minute later to an instant a minute later, a blind
	// refusal resolves to a bounded guess from the moment it is read, and a
	// restore puts back an instant that is already the row's. So the wall the
	// row is on stands while it is still AHEAD.
	//
	// The clause that is NOT per wall, and the only one that can see a defect
	// which invents a wall per park: every per-wall bound (one notification, a
	// budget of probes, one notice) is satisfied by a run whose every park
	// opens a new wall, while its owner, its operator and its pull request
	// hear from it once per park and the probe pass pulls it once per pass.
	//
	// A wall that has PASSED is spent news and may be replaced: the window it
	// named is over, and the park that follows is a new wait whatever named
	// it.
	if st.IsPark() && !st.Dated() && armedHere(before, after) &&
		before.Row != nil && before.Row.Wall != nil && before.Row.Wall.After(now) &&
		wallMoved(before.Row, after.Row) {
		fail("I9", "a park whose refusal named no instant of its own moved the wall from %s to %s: one provider window then collects a notification, an operator incident, a pull-request notice and a fresh probe budget per PARK, and no per-wall bound can see it",
			instantPtr(before.Row.Wall), instantPtr(after.Row.Wall))
	}

	// I5b-for-usernotify — the run's OWNER hears about a wall once, and about
	// every wall. Every park, probe pull, lease and free re-arm rewrites the
	// row and moves updated_at, so a per-transition episode pushed one message
	// per bookkeeping write — about twenty-five on a weekly wall at the
	// shipped probe budget, each of them saying the run had failed.
	//
	// Counted per WALL rather than per step, because a run leaves its wall and
	// comes back to it (a resume clears retry_after, the park re-arms the same
	// instant) and a counter that reset on the way out would bless a second
	// message on the way back. A notification delivered while the row is
	// parked on NO wall is an ordinary outcome and not this invariant's
	// business.
	wallBefore := before.ParkedWall()
	if delivered := after.UserNotices - before.UserNotices; delivered > 0 && !wallBefore.IsZero() {
		if n := l.notifiedWall(wallBefore) + delivered; n > 1 {
			fail("I5", "the run's owner has now been notified %d times about the wall at %s: one provider window collects a park, a pull and a free re-arm per probe of the budget, and nothing new happened to the run",
				n, instant(wallBefore))
		}
	}
	if st.Kind == StepUserNotify && !wallBefore.IsZero() &&
		l.notifiedWall(wallBefore) == 0 && after.UserNotices == before.UserNotices {
		fail("I5", "the park that opened the wall at %s notified nobody: suppressing it leaves the run's owner with a run that looks dead for days",
			instant(wallBefore))
	}

	// Ledger bookkeeping the predicates above read. A PAID arming is what
	// opens a wall — whoever performed it, and whatever it was called: it
	// charges an attempt and closes the probe ledger, which is exactly what
	// the notice reads. A free re-arm opens none: it re-states a wall the
	// pull request already heard.
	if after.Attempts() > before.Attempts() {
		l.WallsOpened++
	}
	if st.IsResume() && after.Resumes > before.Resumes && !liveBefore {
		l.TokensSpent++
	}
	// What the owner has heard, booked on the wall the row was parked on when
	// the step ran.
	if delivered := after.UserNotices - before.UserNotices; delivered > 0 && !wallBefore.IsZero() {
		if l.notified == nil {
			l.notified = map[int64]int{}
		}
		l.notified[wallBefore.Unix()] += delivered
	}
	// And the wall the NEXT step owes: what this one legitimately left the row
	// holding. A row holding nothing owes nothing — it is exhausted, retired
	// or executing, and whether it should have been is I4's question, not
	// I2's.
	if held := after.Held(); held.Before(l.Wall) {
		l.Wall = held
	}
	return bad
}

// dueAndUnleased reports a row the due pass must act on: armed for an
// instant that has arrived, still resumable, and under no live claim.
func dueAndUnleased(s Snapshot, now time.Time) bool {
	if s.Row == nil || s.Row.RetryAfter == nil || s.Status != store.RunStatusFailedResumable {
		return false
	}
	if s.Row.RetryAfter.After(now) {
		return false
	}
	return s.Row.ClaimedAt == nil || !s.Row.ClaimedAt.After(now.Add(-ClaimLease))
}

// asStored is an instant as the database would hold it: UTC, truncated to the
// millisecond BSON stores. Every comparison between what a SEQUENCE named and
// what a ROW carries goes through it.
func asStored(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

func instant(t time.Time) string {
	if t.IsZero() {
		return "nothing"
	}
	return t.UTC().Format(time.RFC3339)
}

func instantPtr(t *time.Time) string {
	if t == nil {
		return "nothing"
	}
	return instant(*t)
}

func describe(r *store.RunRetryState) string {
	if r == nil {
		return "no retry state"
	}
	return fmt.Sprintf("retry_after=%s probe_from=%s probe_count=%d last_probe=%s attempts=%d reason=%s",
		instantPtr(r.RetryAfter), instantPtr(r.ProbeFrom), r.ProbeCount, instantPtr(r.LastProbeAt), r.Attempts, r.Reason)
}

func describeSteps(steps []Step) string {
	parts := make([]string, 0, len(steps))
	for _, s := range steps {
		parts = append(parts, s.String())
	}
	return strings.Join(parts, " → ")
}
