//go:build windows

package proc

import "os/exec"

// DetachProcessGroup is a no-op on Windows. The Unix PGID semantics
// don't have a direct analogue; the failure modes the Unix variant
// guards against (watchexec PGID-cascade kills) don't apply on
// Windows hosts.
func DetachProcessGroup(_ *exec.Cmd) {}
