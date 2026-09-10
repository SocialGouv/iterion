//go:build windows

package pisdk

import (
	"os"
	"os/exec"
)

// hardenSubtreeTermination is a no-op on Windows: there is no process-group
// signal, so os/exec's default (terminate the leader) stands and pi's own
// children survive cancellation. A Job Object
// (CREATE_NEW_PROCESS_GROUP + TerminateJobObject) is the path forward, the
// same one documented on claudesdk.setProcessGroup.
func hardenSubtreeTermination(_ *exec.Cmd) {}

// killSubtree degrades to killing the leader alone on Windows.
func killSubtree(pid int) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
