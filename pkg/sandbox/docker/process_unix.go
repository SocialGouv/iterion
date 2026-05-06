//go:build unix

package docker

import (
	"os/exec"
	"syscall"
)

// detachProcessGroup makes the subprocess the leader of its own
// process group so a SIGTERM delivered to the iterion editor's PGID
// (e.g. via `watchexec -r`) does not propagate and kill an in-flight
// `docker create` or `docker exec` call mid-write. Mirrors the
// pattern used by pkg/runtime/worktree.go for git invocations.
func detachProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
