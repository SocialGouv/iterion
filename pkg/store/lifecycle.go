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
	FailureNodeNotFound   FailureCode = "NODE_NOT_FOUND"
	FailureNoOutgoingEdge FailureCode = "NO_OUTGOING_EDGE"
	// FailureLoopExhausted, FailureJoinFailed and FailureResumeInvalid
	// are RESERVED: declared for the event vocabulary but with no
	// persisting writer today — a run record never carries them until
	// their sites are classified. Kept so the wire vocabulary and the
	// runtime aliases stay stable.
	FailureLoopExhausted    FailureCode = "LOOP_EXHAUSTED"
	FailureBudgetExceeded   FailureCode = "BUDGET_EXCEEDED"
	FailureExecutionFailed  FailureCode = "EXECUTION_FAILED"
	FailureWorkspaceSafety  FailureCode = "WORKSPACE_SAFETY"
	FailureTimeout          FailureCode = "TIMEOUT"
	FailureCancelled        FailureCode = "CANCELLED"
	FailureJoinFailed       FailureCode = "JOIN_FAILED"
	FailureResumeInvalid    FailureCode = "RESUME_INVALID"
	FailureSchemaValidation FailureCode = "SCHEMA_VALIDATION"
	FailureRateLimited      FailureCode = "RATE_LIMITED"
	// FailureUsageLimitBlocked: the provider's subscription/quota WINDOW
	// is exhausted (Anthropic forfait 5h / session / weekly cap) —
	// distinct from FailureRateLimited because retrying inside the
	// window can never succeed: the only cure is waiting for the reset.
	// In-node recovery fails terminal immediately; the run lands
	// failed_resumable and the run-level auto-resume loop waits with a
	// reset-aware delay (see pkg/cli/auto_resume.go).
	FailureUsageLimitBlocked     FailureCode = "USAGE_LIMIT_BLOCKED"
	FailureContextLengthExceeded FailureCode = "CONTEXT_LENGTH_EXCEEDED"
	FailureToolFailedTransient   FailureCode = "TOOL_FAILED_TRANSIENT"
	FailureToolFailedPermanent   FailureCode = "TOOL_FAILED_PERMANENT"
	// FailureNetworkTransient: occasional ISP / DNS / TCP / TLS hiccup
	// reaching the upstream model API. Distinct from
	// FailureExecutionFailed so the recovery dispatcher can apply a
	// longer exponential-backoff budget — a 2-second single retry is
	// plenty for "stale token" or "race on the tool subprocess", but
	// useless against a 30-second captive-portal handoff or a
	// multi-minute datacenter routing blip.
	FailureNetworkTransient FailureCode = "NETWORK_TRANSIENT"
	// FailureAuthFailed: the upstream model provider rejected the
	// request for credential reasons (HTTP 401/403, expired token,
	// invalid api key). NOT transient — retrying the same call can
	// never succeed until a human re-authenticates. The recovery
	// dispatcher pauses for human instead of burning the retry budget;
	// the run is resumable once the credential is refreshed.
	FailureAuthFailed FailureCode = "AUTH_FAILED"

	// FailureInterrupted: an INTERNAL stop (runner drain, dispatcher
	// stall reap, server shutdown) parked the run failed_resumable.
	// Previously only visible as the run_failed event's
	// `interrupted:true` flag.
	FailureInterrupted FailureCode = "INTERRUPTED"
	// FailureFailNode: the workflow's own `fail` node ended the run —
	// a deliberate graph termination, not a crash (ADR-015). It is the
	// UNTYPED outcome: a `fail <name>:` declaration supplies its own
	// UPPER_SNAKE code instead, so this constant means "the bot refused
	// and did not say why", which is exactly what it should read as.
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
	// FailureDLQParked: the queue exhausted its deliveries for this run
	// and parked it on the DLQ — replay via /api/admin/dlq.
	FailureDLQParked FailureCode = "DLQ_PARKED"
	// FailureLaunchFailed: the run never left the launch path. Its row was
	// persisted but no queue message was ever published for it (the
	// publish, the IR encoding or the contribution payload failed), so no
	// runner will ever claim it; the error names the step. Terminal — the
	// launch is re-done by its caller (a forge redelivery, a new click),
	// never by resuming this row.
	FailureLaunchFailed FailureCode = "LAUNCH_FAILED"
	// FailureSandboxSetupTimeout: a sandbox driver's bounded SETUP
	// phase (workspace copy, git fixup, …) exceeded its per-phase
	// budget. Distinct from FailureTimeout (a node's deadline — raising
	// max_duration is not the cure here) and from FailureInterrupted
	// (runner drain / lost heartbeat): the phase ran on a
	// driver-internal child ctx with a driver-internal bound while the
	// run ctx stayed live. Persisted on RunStatusFailedResumable so the
	// redelivery lands on a healthy pod, where a stuck kubectl-exec pipe
	// routinely clears.
	FailureSandboxSetupTimeout FailureCode = "SANDBOX_SETUP_TIMEOUT"
	// FailureSandboxCapacity: the sandbox never STARTED because the
	// cluster had no room for its pod (unschedulable past the start
	// deadline, or still being brought up on the node it landed on).
	// Distinct from FailureSandboxSetupTimeout, which is a phase that RAN
	// and stalled: here nothing of the run executed at all, so the retry
	// is a fresh placement rather than a resume of half-done work — and
	// distinct from a terminal sandbox failure (a bad image reference, an
	// invalid spec), which re-fails identically on every pod. Persisted on
	// RunStatusFailedResumable so an hourly sentinel does not silently
	// lose its tick when the fleet sits at its request ceiling.
	FailureSandboxCapacity FailureCode = "SANDBOX_CAPACITY"
)

// AllRunStatuses is the exhaustive status vocabulary, for callers that
// derive a policy set from a predicate (and for the truth-table tests).
// Order matches the declaration block above.
var AllRunStatuses = []RunStatus{
	RunStatusRunning, RunStatusPausedWaitingHuman, RunStatusPausedOperator,
	RunStatusFinished, RunStatusFailed, RunStatusFailedResumable,
	RunStatusCancelled, RunStatusQueued,
}

// CarriesFailureCode names the only statuses on which a non-empty
// FailureCode may persist. Every status transition through the store
// choke points clears the field when the target is outside this set,
// which is what makes a stale code impossible after a resume.
func (s RunStatus) CarriesFailureCode() bool {
	return s == RunStatusFailed || s == RunStatusFailedResumable || s == RunStatusCancelled
}

// HoldsCredentialSlot names the statuses that count toward a
// credential's concurrency ceiling (secrets.ApiKey.MaxConcurrentRuns):
// running is spending, and queued is about to — admitting a burst of
// queued runs against a full key is exactly the thundering herd the
// ceiling exists to stop. Parked (failed_resumable) and paused runs
// hold NO slot: they spend nothing while they wait, and their resume
// re-resolves credentials against the ceiling like any claim.
func (s RunStatus) HoldsCredentialSlot() bool {
	return s == RunStatusRunning || s == RunStatusQueued
}

// CarriesPausePointer reports whether a run in this status may
// truthfully carry a pending-interaction pointer on its checkpoint
// (Checkpoint.InteractionID / InteractionQuestions). True for the
// paused statuses (the pointer IS the pause) and for queued — the
// in-flight cloud resume hop: SubmitResume flips paused → queued
// before a runner claims the message, and the runner's queued router
// reads the pointer to route a human-answers resume. Every other
// transition consumes it: the checkpoint itself survives (ADR-095 §5),
// but a pointer on a cancelled/terminal/running run is a replayable
// lie — a later resume would route back into the pause path and cross
// the human gate with empty answers.
func (s RunStatus) CarriesPausePointer() bool {
	return s.IsPaused() || s == RunStatusQueued
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
// machinery (--auto-resume, usage-window retries) resume this run with
// no human in the loop? Deliberately excludes cancelled — an operator's
// cancel is a decision automation must never override — and the paused
// statuses, which wait on a human by definition. One documented
// divergence: the dispatcher's resumableRunID additionally re-dispatches
// its OWN paused_operator tickets (pkg/dispatcher/retry.go) — a
// dispatcher-owned pause is machinery state there, not an operator's;
// do not "align" it onto this predicate.
func (s RunStatus) CanAutoResume() bool { return s == RunStatusFailedResumable }

// CountsAgainstLaunchLimit: the run occupies launch-admission capacity
// (tenant concurrency caps, overlap policies). Named after the policy,
// not "active": a paused run is alive too, it just doesn't hold a
// launch slot.
func (s RunStatus) CountsAgainstLaunchLimit() bool {
	return s == RunStatusQueued || s == RunStatusRunning
}

// CanBeCancelled is the MAXIMAL set a cancel write may stomp: anything
// not already finally settled (finished, failed, cancelled). Surfaces
// with a narrower reach keep their own subset and say why — the
// engine's ctx-cancel CAS excludes queued because a queued doc is a
// NEWER attempt that engine does not own (pkg/runtime/run_failure.go),
// and runview's CancelInactive excludes running (a live run is
// cancelled through its process, not a store write).
func (s RunStatus) CanBeCancelled() bool {
	return !s.IsFinalSuccess() && !s.IsFinalFailure() && s != RunStatusCancelled
}
