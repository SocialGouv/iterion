package dispatcher

import (
	"fmt"

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
func DecideStuckCard(run *store.Run, runErr error) StuckDecision {
	if runErr != nil {
		return StuckDecision{StuckKeep, fmt.Sprintf("run unreadable (%v) — a read error conserves", runErr)}
	}
	if run == nil {
		return StuckDecision{StuckReleaseOnly, "claim held but no run was ever recorded — the claimant died pre-launch"}
	}
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
