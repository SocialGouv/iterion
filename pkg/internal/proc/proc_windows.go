//go:build windows

package proc

import "os/exec"

// DetachProcessGroup is a no-op on Windows. The Unix PGID semantics
// don't have a direct analogue; the failure modes the Unix variant
// guards against (watchexec PGID-cascade kills) don't apply on
// Windows hosts.
func DetachProcessGroup(_ *exec.Cmd) {}

// TerminateGroupOnCancel is a no-op on Windows. This is an explicit
// DEGRADATION, not parity: cancellation keeps os/exec's default
// behaviour of killing the direct child only, so a descendant that child
// spawned survives and can hold the output pipes. Closing the gap needs a
// Job Object (CREATE_NEW_PROCESS_GROUP at spawn + TerminateJobObject on
// cancel), not a negative-PID signal, which Windows has no equivalent of.
// The closest approximation already in the tree is
// claudesdk.killProcessGroup's Windows arm, which shells out to
// `taskkill /F /T`.
func TerminateGroupOnCancel(_ *exec.Cmd) {}

// KillProcessGroup is a no-op on Windows, for the reason
// [TerminateGroupOnCancel] gives: there is no negative-PID signal to
// deliver it with, so no group is ever reported killed. The same Job
// Object seam is the one that would close it.
func KillProcessGroup(_ *exec.Cmd) (bool, error) { return false, nil }
