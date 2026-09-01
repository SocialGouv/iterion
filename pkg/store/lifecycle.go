package store

// This file is the canonical terminal-state contract for runs (ADR-095).
//
// Two things live here and nowhere else:
//
//  1. The policy predicates on RunStatus. Each one answers ONE named
//     question; callers pick the predicate whose question they are
//     actually asking instead of re-deriving a status set. Predicates
//     that other layers deliberately diverge from (supervise's
//     event-level terminal set, runview's ExecStatus monotonicity set,
//     the board/pipeline sets) document the divergence at their own
//     declaration and are pinned by the cross-layer agreement tests.
//
//  2. FailureCode — the persisted, machine-readable classification of
//     why a run is in a failure status. It is the same vocabulary the
//     engine uses in-process (runtime.ErrorCode is a type alias of it),
//     persisted on Run.FailureCode instead of dying at the store
//     boundary as free text.

// FailureCode classifies a run failure. The empty value means UNKNOWN
// (legacy rows, writers not yet classified) — never "no failure": the
// presence of a failure is Run.Status's job.
//
// The registry below is OPEN-WORLD: it documents the codes iterion
// itself emits, but readers MUST NOT validate against it — a newer
// binary (or an external writer) may persist a code this binary does
// not know, and it must round-trip unharmed. Zero-means-unknown is the
// only universal rule.
type FailureCode string

const (
	FailureNodeNotFound          FailureCode = "NODE_NOT_FOUND"
	FailureNoOutgoingEdge        FailureCode = "NO_OUTGOING_EDGE"
	FailureLoopExhausted         FailureCode = "LOOP_EXHAUSTED"
	FailureBudgetExceeded        FailureCode = "BUDGET_EXCEEDED"
	FailureExecutionFailed       FailureCode = "EXECUTION_FAILED"
	FailureWorkspaceSafety       FailureCode = "WORKSPACE_SAFETY"
	FailureTimeout               FailureCode = "TIMEOUT"
	FailureCancelled             FailureCode = "CANCELLED"
	FailureJoinFailed            FailureCode = "JOIN_FAILED"
	FailureResumeInvalid         FailureCode = "RESUME_INVALID"
	FailureSchemaValidation      FailureCode = "SCHEMA_VALIDATION"
	FailureRateLimited           FailureCode = "RATE_LIMITED"
	FailureUsageLimitBlocked     FailureCode = "USAGE_LIMIT_BLOCKED"
	FailureContextLengthExceeded FailureCode = "CONTEXT_LENGTH_EXCEEDED"
	FailureToolFailedTransient   FailureCode = "TOOL_FAILED_TRANSIENT"
	FailureToolFailedPermanent   FailureCode = "TOOL_FAILED_PERMANENT"
	FailureNetworkTransient      FailureCode = "NETWORK_TRANSIENT"
	FailureAuthFailed            FailureCode = "AUTH_FAILED"

	// FailureInterrupted: an INTERNAL stop (runner drain, dispatcher
	// stall reap, server shutdown) parked the run failed_resumable.
	// Previously only visible as the run_failed event's
	// `interrupted:true` flag.
	FailureInterrupted FailureCode = "INTERRUPTED"
	// FailureFailNode: the workflow's own `fail` node ended the run —
	// a deliberate graph termination, not a crash (ADR-015).
	FailureFailNode FailureCode = "FAIL_NODE"
	// FailureProcessOrphaned: a liveness probe (flock, lease, pid)
	// found the run's owner dead and flipped it failed_resumable.
	// Declared here for the orphan sweep/reconcile writers; wiring
	// them is follow-up work to the ADR-095 slice.
	FailureProcessOrphaned FailureCode = "PROCESS_ORPHANED"
	// FailureQueueSchemaMismatch: a cloud queue delivery was parked
	// because its message schema version is outside the runner's
	// accepted range. Declared for the schema-park writer (follow-up).
	FailureQueueSchemaMismatch FailureCode = "QUEUE_SCHEMA_MISMATCH"
)

// KnownFailureCodes documents the codes this binary emits. For humans
// and docs generation only — never use it to validate a persisted
// value (open-world contract above).
var KnownFailureCodes = []FailureCode{
	FailureNodeNotFound, FailureNoOutgoingEdge, FailureLoopExhausted,
	FailureBudgetExceeded, FailureExecutionFailed, FailureWorkspaceSafety,
	FailureTimeout, FailureCancelled, FailureJoinFailed, FailureResumeInvalid,
	FailureSchemaValidation, FailureRateLimited, FailureUsageLimitBlocked,
	FailureContextLengthExceeded, FailureToolFailedTransient,
	FailureToolFailedPermanent, FailureNetworkTransient, FailureAuthFailed,
	FailureInterrupted, FailureFailNode, FailureProcessOrphaned,
	FailureQueueSchemaMismatch,
}

// CarriesFailureCode names the only statuses on which a non-empty
// FailureCode may persist. Every status transition through the store
// choke points clears the field when the target is outside this set,
// which is what makes a stale code impossible after a resume.
func (s RunStatus) CarriesFailureCode() bool {
	return s == RunStatusFailed || s == RunStatusFailedResumable || s == RunStatusCancelled
}

// ---------------------------------------------------------------------------
// Policy predicates — one named question each
// ---------------------------------------------------------------------------

// IsFinalSuccess: the run completed its workflow (reached a done node).
func (s RunStatus) IsFinalSuccess() bool { return s == RunStatusFinished }

// IsFinalFailure: the run failed with no automatic path forward — only
// an explicit operator action (rewind, fresh launch) touches it again.
func (s RunStatus) IsFinalFailure() bool { return s == RunStatusFailed }

// IsTerminalResumable: terminal for polling purposes (IsTerminal is
// true) yet holding a checkpoint an operator may resume from. The
// deliberate ambiguity of failed_resumable/cancelled being "terminal",
// made explicit.
func (s RunStatus) IsTerminalResumable() bool {
	return s == RunStatusFailedResumable || s == RunStatusCancelled
}

// IsQueued: submitted to the cloud queue, not yet claimed by a runner.
func (s RunStatus) IsQueued() bool { return s == RunStatusQueued }

// CanOperatorResume answers the EXTERNAL eligibility question: may an
// operator ask this run to continue (studio Resume, `iterion resume`,
// SubmitResume, MCP local_resume)? It deliberately says nothing about
// HOW the resume proceeds — paused_waiting_human routes through the
// answers path (see RequiresResumeAnswers) while the other three
// restart from the failure checkpoint — and internal claim CAS sets
// keep their own, narrower or wider, sets (e.g. the failure-resume
// claim also accepts `queued` for the cloud pre-flip, and the pause
// claim accepts only paused_waiting_human).
func (s RunStatus) CanOperatorResume() bool {
	return s == RunStatusFailedResumable || s == RunStatusCancelled ||
		s == RunStatusPausedOperator || s == RunStatusPausedWaitingHuman
}

// RequiresResumeAnswers: resuming this status needs the pending human
// interaction answered first (`--answers-file`, the studio form).
func (s RunStatus) RequiresResumeAnswers() bool { return s == RunStatusPausedWaitingHuman }

// CanAutoResume answers the AUTOMATIC eligibility question: may
// machinery (dispatcher retry, --auto-resume, usage-window retries)
// resume this run with no human in the loop? Deliberately excludes
// cancelled — an operator's cancel is a decision automation must never
// override — and the paused statuses, which wait on a human by
// definition.
func (s RunStatus) CanAutoResume() bool { return s == RunStatusFailedResumable }

// CountsAgainstLaunchLimit: the run occupies launch-admission capacity
// (tenant concurrency caps, overlap policies). Named after the policy,
// not "active": a paused run is alive too, it just doesn't hold a
// launch slot.
func (s RunStatus) CountsAgainstLaunchLimit() bool {
	return s == RunStatusQueued || s == RunStatusRunning
}
