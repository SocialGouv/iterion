//go:build unix

package kubernetes

import (
	"os/exec"
	"syscall"
)

// detachProcessGroup makes the subprocess the leader of its own
// process group so a SIGTERM to the runner pod doesn't propagate and
// kill an in-flight `kubectl apply` mid-write. Mirrors the docker
// driver's matching helper.
func detachProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
