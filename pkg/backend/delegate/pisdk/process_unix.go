//go:build unix

package pisdk

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// hardenSubtreeTermination makes the spawned pi process the leader of its own
// group and widens ctx cancellation to that whole group.
//
// pi is a coding agent: it forks shells, language servers and MCP servers of
// its own, and they inherit the stdout/stderr pipes this client reads. Killing
// only the leader leaves them running and the read loop blocked — cancelling
// the node would stop the wait without stopping the work.
//
// Pre-Start. Only the Cancel half needs a context; the Setpgid half is what
// makes killSubtree addressable either way.
func hardenSubtreeTermination(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killSubtree(cmd.Process.Pid)
	}
}

// killSubtree SIGKILLs every process in the group led by pid. Returns
// os.ErrProcessDone when the group is already gone, which os/exec reads as
// "nothing to interrupt" rather than an error to report.
func killSubtree(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
