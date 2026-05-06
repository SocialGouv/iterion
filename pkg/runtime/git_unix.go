//go:build unix

package runtime

import (
	"os/exec"
	"syscall"
)

// detachGitProcessGroup puts a `git` child in its own process group via
// Setpgid. A SIGTERM delivered to the editor's PGID — for example by
// `watchexec -r` rebuilding the dev-mode backend mid-merge — therefore
// does NOT reach the in-flight `git commit` and abort it with
// "signal: terminated".
//
// We deliberately stop short of Setsid: the runtime never wants to
// signal git via -pid, and a fresh session would also detach from the
// controlling terminal — fine for the deferred-merge HTTP path but
// noisy for CLI runs that route progress through the parent's stderr.
func detachGitProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
