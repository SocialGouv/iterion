# ADR-095 — Canonical terminal-state contract and persisted failure taxonomy

- Status: accepted (2026-09-01)
- Relations: ADR-014 (paused-run semantics — deliberately NOT a transition
  state machine; this ADR adds classification only, zero new transitions),
  ADR-028 (dispatcher lifecycle), ADR-015 (FailNode handling — the FAIL_NODE
  code is the typed signal its deferral 5 waited for), PR #594 (the
  operator-cancel / internal-interruption split the CANCELLED vs INTERRUPTED
  codes persist).

## Problem

Terminal-ness was decided by four disagreeing predicates (store run-status,
supervise event-level, runview ExecStatus, ir graph-shape), the resumable
4-set {failed_resumable, cancelled, paused_operator, paused_waiting_human}
was hand-copied at 7+ sites, and failure classification — a typed, 18-value
`runtime.ErrorCode` — died at the persistence boundary: `Run.Error` is free
text, so every downstream consumer (alerts, retry lanes, dispatchers) either
string-matched or stayed blind. Audited in full (146 status-decision sites,
110 policy-carrying, 8 concrete contradictions) before this contract.

## Decision

### 1. The contract lives in `pkg/store` ([lifecycle.go](../../pkg/store/lifecycle.go))

`pkg/store` is the shared floor of the import DAG (store ← {runtime,
supervise} ← runview ← dispatcher) and already owns the `RunStatus` enum.
No new leaf package: `runtime.ErrorCode` becomes a **type alias** of
`store.FailureCode` (constants re-exported), so one vocabulary exists.

### 2. Policy-named predicates, not one IsTerminal

Each predicate answers ONE question; callers stop re-deriving sets:
`IsTerminal` (stop polling — unchanged), `IsPaused`, `IsFinalSuccess`,
`IsFinalFailure`, `IsTerminalResumable`, `IsQueued`, `CanOperatorResume`
(+`RequiresResumeAnswers`), `CanAutoResume` (never cancelled — automation
must not override an operator's stop), `CountsAgainstLaunchLimit`.

**Documented divergences that STAY** (each argued at its declaration and
pinned by tests): supervise's event-level terminal set (failed_resumable
collapses into run_failed; run_paused non-terminal), runview's ExecStatus
monotonicity set (paused is settle-only-downgradable), `pipelineRunRetired`
(narrower: failed_resumable still holds a slot), `sandboxContainerReapable`
(wider: paused_operator containers reapable), worktreepool's ownership
question, the internal claim-CAS sets (they gate a TRANSITION, not
eligibility — e.g. the failure-resume claim accepts `queued` for the cloud
pre-flip). The negative-space test
([lifecycle_negative_space_test.go](../../pkg/store/lifecycle_negative_space_test.go))
forbids NEW hand-rolled sets in store/supervise/runview outside its
per-file, reason-carrying allowlist.

### 3. `Run.FailureCode` — persisted, open-world, zero-means-unknown

`failure_code` (json+bson, omitempty) classifies **the cause of the CURRENT
failure status** — it annotates `Run.Error` and follows it exactly:

- Written by the same store operation as the status (`FailRun*` carry the
  code; `UpdateRunStatusCoded` / `UpdateRunStatusIfCoded` for the plain and
  CAS paths — never a separate read-modify-write).
- **Cleared by every transition to a non-failure status** — `running`,
  `queued` (the cloud resume pre-flip), paused, finished. The SubmitResume
  rollback restores the prior code with the prior text. `healRun` repairs a
  stale code a whole-document writer resurrects.
- **Open-world**: the registry (`KnownFailureCodes`) is documentation.
  Readers never validate against it; an unknown non-empty code round-trips
  unharmed (conformance + BSON decode-shape tests). Empty = UNKNOWN, never
  "no failure".
- Writer-by-writer semantics: engine failures persist their
  `RuntimeError.Code`; FailNode → `FAIL_NODE` (deliberate termination,
  distinct from a crash at last); internal stops → `INTERRUPTED`; operator
  cancels (runtime CAS, publisher, runview, setup) → `CANCELLED`; node
  timeout → `TIMEOUT`; the runner usage-cap park → `USAGE_LIMIT_BLOCKED`.
  **Synthetic parkings write an empty code** (fork shell, rewind/restore
  claims): they are not failures. A cancel of an already-parked run writes
  CANCELLED; the prior cause survives in the composed text, as before.
- Declared-but-deferred writers: `PROCESS_ORPHANED` (orphan sweeps,
  reconciles, force-stale, dead-pid), `QUEUE_SCHEMA_MISMATCH` (schema park),
  the DLQ park — a follow-up card wires them; until then those paths
  honestly persist unknown.

### 4. Mixed-fleet note

An older binary's whole-document `SaveRun` (Mongo `ReplaceOne`) drops the
field it does not know. Accepted WITHOUT a two-release rollout: the field is
advisory (unknown is a first-class reading), the loss window is the rolling
deploy, and `healRun` cleans what it can. Deploy server and runner together
(the runner image is digest-pinned; bump it in the same infra change).
Rollback is safe: the field stops being written, stale values heal on read.

## Consequences

- The cloud publisher's cancel CAS gained `paused_operator` (the one set
  missing it — a cloud run parked there was un-cancellable, "cancel raced"
  forever). Other contradictions from the audit are follow-up cards, not
  silent drift: they are now enumerated with reasons.
- Operator alerts and the run API name the failure class
  (`failure_code` on RunSummary/RunHeader, `[CODE]` on the alert line);
  consumers that string-match `Run.Error` (dispatcher sandbox-classify,
  retry lanes) can migrate to typed-first reads as follow-up.
- Cross-layer agreement is executable: predicate truth tables + relation
  invariants in store, event-collapse documentation in supervise,
  ExecStatus divergence in runview.
