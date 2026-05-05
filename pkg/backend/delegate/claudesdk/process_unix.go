//go:build !windows

package claudesdk

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup makes the spawned subprocess the leader of a new process
// group. Without this, signals sent to the subprocess do not reach its
// descendants — and the Claude CLI routinely spawns long-lived MCP servers
// and background bash jobs (via the Bash `run_in_background` and Monitor
// tools). When the SDK falls silent and we need to abort, we want the entire
// subtree to die, not just the leader.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup signals every process in the group whose leader has the
// given pid. `pid` must be the leader of its own group (see setProcessGroup),
// otherwise Kill(-pid, …) would target an unrelated group.
//
// Returns nil when the group is already gone (ESRCH), so callers can use this
// as an idempotent best-effort kill.
func killProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return nil
}
