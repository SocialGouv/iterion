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
