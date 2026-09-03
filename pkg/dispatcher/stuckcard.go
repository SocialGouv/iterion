package dispatcher

import (
	"fmt"
	"slices"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/store"
)

// StuckAction is what the watchdog does with a card whose claim expired
// (or was found ownerless): nothing, a terminal filing, or a return to
// the eligible pool. The DECISION is this pure table; the ACTING is the
// reaper's, under a recovery claim — never clear-then-decide (the
// double-launch window the plan review's F9 closed).
type StuckAction int

const (
	// StuckKeep: preserve everything — the run is alive, paused (ADR-014:
	// a retained claim is a legitimate long-term state), operator-
	// cancelled, owned by a platform continuation, or unreadable (every
	// read error conserves, the ADR-070 doctrine).
	StuckKeep StuckAction = iota
	// StuckComplete: the run finished its workflow — file the card as
	// completed (the same move a live finish worker would have made).
	StuckComplete
	// StuckFail: the run failed terminally — file the card as failed.
	StuckFail
	// StuckRepark: the run is terminal-resumable with no platform
	// continuation armed — release the claim and leave the card for the
	// ordinary retry machinery (the card re-enters the eligible pool and
	// resolveRunID resumes the recorded run, never a fresh sibling).
	StuckRepark
	// StuckReleaseOnly: the claimant died before leaving any run — free
	// the claim so the card is simply eligible again.
	StuckReleaseOnly
)

func (a StuckAction) String() string {
	switch a {
	case StuckComplete:
		return "complete"
	case StuckFail:
		return "fail"
	case StuckRepark:
		return "repark"
	case StuckReleaseOnly:
		return "release"
	default:
		return "keep"
	}
}

// StuckDecision is one row's verdict plus its evidence.
type StuckDecision struct {
	Action StuckAction
	Reason string
}

// ShouldFileStuckCard decides whether the watchdog may write `target`
// onto a card it has just reclaimed. One definition for both reapers
// (local FS and cloud Mongo): a recopied guard is how the two drift.
//
// It reproduces the live finish worker's contract rather than inventing
// a second one (maybeTransitionToCompleted):
//
//   - an empty target, or one equal to the running state, means the
//     operator disabled the auto-transition — leave the card be;
//   - a card no longer in the running state was moved DELIBERATELY (an
//     operator re-queueing it, a bot with board.move). That intent
//     predates the watchdog and outranks its default filing —
//     "already moved the state. Honor it."
//
// The exception that makes the guard usable at all: BOTH launch paths
// move a claimed card into the running column BEST-EFFORT (the local one
// says so — "continue regardless, claim is already taken"). So a card
// can be claimed, run to completion, and still sit in the column it was
// launched FROM. That is not an intent to read, it is a launch that
// never finished moving it — and refusing to file it there leaves it
// eligible, so the next tick launches a second run for delivered work.
// launchStates is that set (the states a card is picked up from).
//
// cardState must be the state observed WITH the ownership — the one
// ReclaimExpired returns — not the one read at listing time, which an
// operator can invalidate before the transfer lands.
func ShouldFileStuckCard(cardState, runningState, target string, launchStates []string) bool {
	if target == "" || target == runningState || cardState == target {
		return false
	}
	if runningState == "" {
		// `running_state: none` is a documented opt-out (the board has no
		// in-flight column). With nothing to compare against, "the card
		// moved" cannot be told from "the card never moved" — so the only
		// safe filing is onto a card still sitting where it was launched
		// from. Anywhere else is somebody's choice, and the watchdog does
		// not overwrite choices it cannot read.
		return slices.Contains(launchStates, cardState)
	}
	if cardState == runningState {
		return true
	}
	return slices.Contains(launchStates, cardState)
}

// DecideStuckCard is the ONE decision table for a card whose claim has
// no live owner — shared by the local reaper and (as the cloud sweeps
// converge on it) the server's stranded-card reconcilers, so two
// authorities can never classify the same situation differently (the
// plan review's F16). Pure: no I/O, no clock — the caller resolves the
// run (promoteIfOrphaned is the liveness oracle that runs FIRST) and
// hands in what it found.
//
// The table, in precedence order — every arm is a test:
//
//	run load error        → Keep      (a read error conserves — ADR-070)
//	no run at all         → Release   (died pre-launch; card re-eligible)
//	running / queued      → Keep      (liveness-first — a live run is
//	                                   never stolen from; the caller's
//	                                   promoteIfOrphaned already had its
//	                                   chance to prove death)
//	paused (any)          → Keep      (ADR-014: the retained claim IS the
//	                                   parking brake)
//	cancelled             → Keep      (an operator's stop is never
//	                                   auto-routed — the same doctrine as
//	                                   the outcome router)
//	continuation armed    → Keep      (redelivery/retry owns the future;
//	                                   acting now races it)
//	finished              → Complete
//	failed (terminal)     → Fail
//	failed_resumable      → Repark    (the ordinary retry machinery
//	                                   resumes the recorded run)
//	anything else         → Keep      (open-world: an unknown status is
//	                                   conserved, never guessed at)
func DecideStuckCard(run *store.Run, runErr error, card StuckCard) StuckDecision {
	if runErr != nil {
		return StuckDecision{StuckKeep, fmt.Sprintf("run unreadable (%v) — a read error conserves", runErr)}
	}
	if run == nil {
		// "No run recorded" only means "died pre-launch" if the card never
		// reached the running column. Stamping the run onto the card is
		// BEST-EFFORT on both launch paths and happens AFTER the launch, so
		// a card already in the running column with no run id is not
		// evidence of death — it is evidence of nothing, and freeing it
		// would put a second run against a worker that may be alive.
		if stampMayStillLand(card) {
			return StuckDecision{StuckKeep,
				"no run recorded, but the card was claimed moments ago and is in the running column — the stamp is best-effort and lands after the launch, so freeing it now could double-launch"}
		}
		return StuckDecision{StuckReleaseOnly, "claim held but no run was ever recorded — the claimant died pre-launch"}
	}
	// Only Repark reaches here: a nil run (the ReleaseOnly row) returned
	// above. Freeing a parked card whose claimant died pre-launch is
	// correct anyway — nothing will pick it up where it sits.
	if d := decideByStatus(run); d.Action == StuckRepark {
		// Returning a card to the pool only makes sense if the card IS in
		// the pool's reach. Parked somewhere deliberate (awaiting_input,
		// review, a hold whose whole brake is the retained claim) it is not
		// waiting to be re-dispatched, and releasing lifts a brake somebody
		// set on purpose.
		if parkedOutOfPool(card) {
			return StuckDecision{StuckKeep, fmt.Sprintf(
				"run %s is %s, but the card sits in %q — parked outside the dispatch pool, so there is nothing to return it to",
				run.ID, run.Status, card.State)}
		}
	}
	return decideByStatus(run)
}

// DecideTransfer is the decision taken BEFORE the claim is taken, and it
// is deliberately narrower than DecideStuckCard: it applies every row
// that protects a live owner — a running run, a parking brake, a run
// stamp still in flight — and NOT the parked-out-of-pool row.
//
// That distinction is what makes the parked row's bound reachable at all.
// Refusing to transfer means refusing to act, and a card nothing acts on
// stays held by its dead owner for ever (in cloud there is no boot sweep
// to free it later). Parking states WHERE the card should sit, not that
// anyone is alive: the run behind it is resumable or absent, so taking
// the claim costs nobody anything — and holding it is precisely what lets
// the watchdog know, one lease later, that it already conserved this card
// once.
func DecideTransfer(run *store.Run, runErr error, card StuckCard) StuckDecision {
	inPool := card
	// Neutralise ONLY the pool-membership inputs; StampWindowOpen and the
	// status rows still speak, because those are the ones about liveness.
	inPool.State, inPool.RunningState = "", ""
	d := DecideStuckCard(run, runErr, inPool)
	// The one liveness row that needs the card, re-applied because the
	// neutralisation above disarmed it: a stamp may still be in flight,
	// and transferring would steal from a live worker. Same predicate as
	// the table's — recopying it is how the two drift, and how each twin
	// ended up covering only one of the copies.
	if runErr == nil && run == nil && stampMayStillLand(card) {
		return StuckDecision{StuckKeep,
			"no run recorded, but the card was claimed moments ago and is in the running column — the stamp is best-effort and lands after the launch, so taking the claim now could steal from a live worker"}
	}
	return d
}

// RecoveryHoldExpired: this card is held under a RECOVERY claim whose
// reason has not cleared, and nothing is working behind it — so the
// "conserved once" bound applies and the hold must come off.
//
// It exists because DecideTransfer's Keep is TERMINAL for the caller: it
// returns before the transfer, so keepAfterTransfer — the only place that
// bound lives — is never reached. For an ordinary claim that is right
// (the Keep protects its live owner). For a claim ALREADY minted by a
// watchdog it is not: a recovery claim protects nobody (a watchdog never
// runs the card's work), it was taken a full lease ago, and it makes the
// card invisible to ListEligible and to every sweep but the one that
// finds it here. Held for ever is the stuck card the watchdog exists to
// clear, wearing the watchdog's own marker.
//
// Releasing files NOTHING, so an operator-cancelled run is not routed —
// the card is simply restored to what it was before a watchdog touched
// it, which is what a release means (ADR §8).
//
// The bound is refused while anything might be alive: a running or queued
// run, a stamp that could still land, or a run the store cannot read
// (unknown is never read as dead — ADR-070). On the local twin the
// running column is itself eligible, so a premature release there is a
// second run against a live one.
func RecoveryHoldExpired(run *store.Run, runErr error, card StuckCard, prev tracker.ClaimToken) bool {
	if !tracker.IsReaperMarker(prev.Marker) {
		return false
	}
	if runErr != nil {
		return false
	}
	if run == nil {
		return !stampMayStillLand(card)
	}
	return run.Status != store.RunStatusRunning && !run.Status.IsQueued()
}

// stampMayStillLand: the card is in the running column and was claimed
// recently enough that its run stamp — written after the launch, and
// best-effort — could still be on its way. ONE definition, read by both
// the table and the pre-transfer decision.
func stampMayStillLand(card StuckCard) bool {
	return card.RunningState != "" && card.State == card.RunningState && card.StampWindowOpen
}

// parkedOutOfPool: the card is neither running nor in a column it would
// be dispatched from, so nothing picks it up if the claim is freed.
func parkedOutOfPool(card StuckCard) bool {
	return card.RunningState != "" && card.State != card.RunningState &&
		!slices.Contains(card.LaunchStates, card.State)
}

// StuckCard is what the watchdog knows about the CARD, as opposed to its
// run. State must be the value observed together with the ownership (the
// one ReclaimExpired returns), never the listing's older copy.
type StuckCard struct {
	State        string
	RunningState string
	LaunchStates []string
	// StampWindowOpen says a run stamp could still plausibly be in
	// flight for this card — the claim was taken moments ago. The stamp
	// is written AFTER the launch and best-effort, so its absence means
	// nothing while this holds, and everything once it does not. Without
	// it the conservative row below never expires, and a card whose
	// stamp will never arrive is held out of the pool forever: the same
	// outcome, for the operator, as the stuck card this watchdog clears.
	StampWindowOpen bool
}

func decideByStatus(run *store.Run) StuckDecision {
	switch {
	case run.Status == store.RunStatusRunning || run.Status.IsQueued():
		return StuckDecision{StuckKeep, fmt.Sprintf("run %s is %s — a live run is never stolen from", run.ID, run.Status)}
	case run.Status.IsPaused():
		return StuckDecision{StuckKeep, fmt.Sprintf("run %s is %s — the retained claim is the parking brake (ADR-014)", run.ID, run.Status)}
	case run.Status == store.RunStatusCancelled:
		return StuckDecision{StuckKeep, fmt.Sprintf("run %s was cancelled by the operator — never auto-routed", run.ID)}
	case run.ContinuationState == store.ContinuationRedeliveryPending || run.ContinuationState == store.ContinuationRetryArmed:
		return StuckDecision{StuckKeep, fmt.Sprintf("run %s carries continuation %q — the platform owns its future", run.ID, run.ContinuationState)}
	case run.Status.IsFinalSuccess():
		return StuckDecision{StuckComplete, fmt.Sprintf("run %s finished — filing the card the way its dead worker would have", run.ID)}
	case run.Status.IsFinalFailure():
		return StuckDecision{StuckFail, fmt.Sprintf("run %s failed terminally (%s)", run.ID, run.FailureCode)}
	case run.Status.IsTerminalResumable():
		return StuckDecision{StuckRepark, fmt.Sprintf("run %s is resumable with no continuation armed — back to the retry machinery", run.ID)}
	default:
		return StuckDecision{StuckKeep, fmt.Sprintf("run %s has status %q this table does not know — conserved, never guessed at", run.ID, run.Status)}
	}
}
