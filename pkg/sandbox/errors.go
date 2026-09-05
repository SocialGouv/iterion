package sandbox

import "errors"

// ErrPhaseTimeout is the sentinel a driver returns when a bounded SETUP
// phase (workspace copy, git fixup, …) exceeds its per-phase budget.
// Distinct from the run's own max_duration and from a
// context.DeadlineExceeded on the run ctx: it fires on a CHILD ctx whose
// deadline is a driver-internal safety bound, while the run ctx stays
// live. Drivers wrap it with errors.Join so the underlying cause and
// context.DeadlineExceeded stay reachable through errors.Is as well.
//
// Consumers:
//
//   - the runtime setup classifier (pkg/runtime setupFailureStatus)
//     persists RunStatusFailedResumable + FailureSandboxSetupTimeout
//     instead of the default hard RunStatusFailed: the stall is a
//     transient infrastructure condition (a stuck kubectl-exec pipe, a
//     rescheduled apiserver) that a fresh pod routinely clears;
//   - the cloud runner's ack policy (pkg/runner classifyExecResult) NAKs
//     the delivery so JetStream redelivers to a healthy pod; a stall that
//     persists through every permitted delivery parks on the DLQ like any
//     other repeated failure.
var ErrPhaseTimeout = errors.New("sandbox: setup phase timeout")

// ErrCapacity is the sentinel a driver returns when the sandbox could not
// be PLACED before its start deadline: the cluster had no room for the
// pod, or the node it landed on was still bringing it up. The run
// executed NOTHING — no node started, no command ran, no side effect
// happened — so the only thing lost is the placement, which a later
// attempt redoes from scratch.
//
// The classification is EVIDENCE-BASED, never a catch-all for "something
// went wrong at sandbox start": a driver returns it only when the cluster
// says the pod was unscheduled or still being created. A broken image
// reference, an invalid spec or a crash-looping container re-fails
// identically on every pod and stays a terminal failure.
//
// Consumers, mirroring ErrPhaseTimeout's:
//
//   - the runtime setup classifier (pkg/runtime setupFailureStatus)
//     persists RunStatusFailedResumable + FailureSandboxCapacity instead
//     of the default hard RunStatusFailed, so the run keeps a road back
//     instead of silently losing a sentinel's tick or a campaign's launch;
//   - the cloud runner's ack policy (pkg/runner classifyExecResult) NAKs
//     the delivery after a delay long enough for a cluster autoscaler to
//     add a node; a cluster that stays full through every permitted
//     delivery parks the run on the DLQ like any other repeated failure.
var ErrCapacity = errors.New("sandbox: no capacity to place the sandbox")
