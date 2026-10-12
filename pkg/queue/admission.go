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
func Admit(msg *RunMessage, run *store.Run) Admission {
	if Superseded(msg, run) {
		return Admission{Drop: DropSupersededAttempt}
	}
	if run.ResumeRequiresExplicit && msg.Resume == nil {
		return Admission{Drop: DropExplicitResumeRequired}
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

// Superseded is the identity rule of a delivery: every transition into
// queued refreshes QueuedAt, and only a publication makes one — so a run
// queued after msg was published belongs to a newer attempt. A document
// without the marker, or a publication time that cannot be read, has no
// identity to tell.
func Superseded(msg *RunMessage, run *store.Run) bool {
	if msg == nil || run == nil || run.QueuedAt == nil {
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
