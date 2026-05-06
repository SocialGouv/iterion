//go:build windows

package docker

import "os/exec"

// detachProcessGroup is a Windows no-op. Docker Desktop on Windows
// runs in WSL2; signal propagation differences mean PGID detachment
// has no equivalent that meaningfully prevents premature termination.
// The container lifecycle is short enough that the failure mode the
// Unix variant guards against is unlikely here.
func detachProcessGroup(_ *exec.Cmd) {}
