//go:build unix

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// DetachProcessGroup makes the subprocess the leader of its own
// process group so a SIGTERM to the parent's PGID doesn't propagate
// to it. Pre-Start; safe to call on any *exec.Cmd.
//
// It says nothing about what happens when the caller's context is
// cancelled — for that, use [TerminateGroupOnCancel] instead.
func DetachProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// TerminateGroupOnCancel makes context cancellation actually terminate the
// subprocess AND everything it spawned. Two gaps in the default
// exec.CommandContext behaviour it closes:
//
//   - the default Cancel kills only the direct child. A shell recipe's
//     background job, or a helper a CLI forked (git-remote-https, ssh),
//     survives and keeps the inherited stdout/stderr pipes open — so the
//     parent's Wait/Output stays blocked on a read that will never EOF.
//     The cancellation then shortens nothing: the run reports cancelled
//     while the work it pays for keeps going.
//
//   - the surviving descendants keep burning wall-clock and, on a cloud
//     runner, a pod — and may still be writing the workspace that
//     worktree finalization and workspace capture are about to read.
//
// The child is made its own group leader (see [DetachProcessGroup]) so the
// negative-PID signal addresses exactly this subtree and nothing else.
// SIGKILL, not SIGTERM: cancellation is not a request.
//
// Pre-Start, and only meaningful on an *exec.Cmd built by
// exec.CommandContext — Cancel is ignored when there is no context. A
// descendant that escapes the group on purpose (its own setsid) is out of
// reach of any signal; cmd.WaitDelay is the only bound there, and it is
// deliberately left to the caller because it also bounds the SUCCESS path
// (a recipe that legitimately backgrounds work and exits 0 would start
// failing with ErrWaitDelay).
func TerminateGroupOnCancel(cmd *exec.Cmd) {
	DetachProcessGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				// The group is already gone: the command finished on its
				// own and cancellation just noticed late. os/exec reads
				// this sentinel as "nothing to interrupt" and leaves the
				// real exit status alone.
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}
