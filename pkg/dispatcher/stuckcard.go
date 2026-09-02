package dispatcher

import (
	"fmt"
	"slices"

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
		if card.RunningState != "" && card.State == card.RunningState && card.StampWindowOpen {
			return StuckDecision{StuckKeep,
				"no run recorded, but the card was claimed moments ago and is in the running column — the stamp is best-effort and lands after the launch, so freeing it now could double-launch"}
		}
		return StuckDecision{StuckReleaseOnly, "claim held but no run was ever recorded — the claimant died pre-launch"}
	}
	if d := decideByStatus(run); d.Action == StuckRepark || d.Action == StuckReleaseOnly {
		// Returning a card to the pool only makes sense if the card IS in
		// the pool's reach. Parked somewhere deliberate (awaiting_input,
		// review, a hold whose whole brake is the retained claim) it is not
		// waiting to be re-dispatched, and releasing lifts a brake somebody
		// set on purpose.
		if card.RunningState != "" && card.State != card.RunningState &&
			!slices.Contains(card.LaunchStates, card.State) {
			return StuckDecision{StuckKeep, fmt.Sprintf(
				"run %s is %s, but the card sits in %q — parked outside the dispatch pool, so there is nothing to return it to",
				run.ID, run.Status, card.State)}
		}
	}
	return decideByStatus(run)
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
