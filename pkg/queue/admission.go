package queue

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Drop is why a runner drops a delivery on admission without executing it.
type Drop string

const (
	// DropSupersededAttempt: the run was queued again after the message was
	// published — only a publication queues a run, so the message belongs to
	// an attempt that is over, whatever the run's status now.
	DropSupersededAttempt Drop = "superseded_attempt"
	// DropExplicitResumeRequired: a rewound run waits for an operator's
	// resume; a launch message does not resume it.
	DropExplicitResumeRequired Drop = "explicit_resume_required"
	// DropCancelled: the run was cancelled after the message was published —
	// the cancel wins over the message.
	DropCancelled Drop = "cancelled"
	// DropDeliberateFailure: the run is parked on a code a redelivery cannot
	// change (DeliberateFailure): the same step would fail identically.
	DropDeliberateFailure Drop = "deliberate_failure"
	// DropSettled: the run finished, failed, or waits for a human's answer —
	// there is nothing for the message to execute.
	DropSettled Drop = "settled"
	// DropOperatorPausedHoldsCapacity: an operator paused the run, and the
	// message carries the places its admission reserved. The pause released
	// them; a redelivery resuming the run would run it on places nobody holds
	// for it, past the cap. The operator's resume re-admits it.
	DropOperatorPausedHoldsCapacity Drop = "operator_paused_holds_capacity"
	// DropRetryArmed: the run is parked with a durable retry armed, and the
	// message is a guarded publication's (RunMessage.Capacity set). The retry
	// sweeper's re-admission resumes it; a redelivery would resume it beside
	// that retry, on places nobody re-admitted.
	DropRetryArmed Drop = "retry_armed"
)

// Admission is a runner's verdict on a delivery of msg, read against the
// run's document: dropped (Drop), or executed — as a resume when AsResume
// says a launch message reached a run that already executed.
type Admission struct {
	Drop     Drop
	AsResume bool
}

// Proceeds reports that the delivery is executed.
func (a Admission) Proceeds() bool { return a.Drop == "" }

// Admit is the admission rule of a delivery of msg on run: the runner's, and
// the DLQ replay's — a replay is only a delivery, and a message the runner
// drops is not replayed, it is refused with the way out. Pure: it reads the
// message and the document and decides.
//
// A launch redelivered after the run already persisted resumable state
// (failed_resumable, paused_operator) proceeds as a resume, so JetStream
// redelivery uses the checkpoint it exists to protect; so does a launch of a
// queued run that carries a checkpoint, which has already executed. A queued
// run's resume publication, a queued first attempt, a running doc (an orphan
// or a live owner, told apart under the run's lock) and any status the rule
// does not know proceed.
//
// A message that carries its admission's places (RunMessage.Capacity) is a
// guarded publication: it never resumes an operator-paused run that holds
// capacity (DropOperatorPausedHoldsCapacity) nor a run whose retry is armed
// (DropRetryArmed), whether it is a launch or a resume — the rule is read
// before either is told apart.
func Admit(msg *RunMessage, run *store.Run) Admission {
	if Superseded(msg, run) {
		return Admission{Drop: DropSupersededAttempt}
	}
	if run.ResumeRequiresExplicit && msg.Resume == nil {
		return Admission{Drop: DropExplicitResumeRequired}
	}
	if msg.Capacity != nil {
		switch {
		case run.Status == store.RunStatusPausedOperator && len(msg.Capacity.Holds) > 0:
			return Admission{Drop: DropOperatorPausedHoldsCapacity}
		case run.Status == store.RunStatusFailedResumable && run.ContinuationState == store.ContinuationRetryArmed:
			return Admission{Drop: DropRetryArmed}
		}
	}
	switch run.Status {
	case store.RunStatusCancelled:
		return Admission{Drop: DropCancelled}
	case store.RunStatusFailedResumable, store.RunStatusPausedOperator:
		if DeliberateFailure(run.FailureCode) {
			return Admission{Drop: DropDeliberateFailure}
		}
		if msg.Resume == nil {
			return Admission{AsResume: true}
		}
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusPausedWaitingHuman:
		return Admission{Drop: DropSettled}
	case store.RunStatusQueued:
		if msg.Resume == nil && run.Checkpoint != nil {
			return Admission{AsResume: true}
		}
	}
	return Admission{}
}

// Superseded compares a quota patch's exact queue marker. For older
// delivery shapes, only the historical publication-time fence is available:
// a marker newer than the publication supersedes it.
func Superseded(msg *RunMessage, run *store.Run) bool {
	if msg == nil || run == nil {
		return false
	}
	if msg.Resume != nil && msg.Resume.CodemodeQuota != nil {
		marker := msg.Resume.CodemodeQuota.QueuedAt
		return marker.IsZero() || !store.MatchesExactQueuedAt(run.QueuedAt, marker)
	}
	if run.QueuedAt == nil {
		return false
	}
	published, err := time.Parse(time.RFC3339Nano, msg.PublishedAtRFC)
	return err == nil && run.QueuedAt.After(published)
}

// DeliberateFailure reports that a run parked on code is refused by any
// redelivery: a bot-defined code — only an operator changing something can
// change the verdict — or an engine code the shared classification calls
// deterministic (pkg/retrypolicy), DLQ_PARKED included. An empty code is
// unknown (legacy rows, paused_operator) and keeps resuming.
func DeliberateFailure(code store.FailureCode) bool {
	return code != "" && (!code.Reserved() || retrypolicy.IsDeterministic(code))
}
