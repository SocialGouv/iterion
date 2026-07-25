//go:build linux

package model

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// configureToolNodeProcessGroup gives a host-executed shell/script recipe its
// own process group and makes context cancellation terminate its entire process
// tree.
//
// A process group alone is insufficient on Linux because a nested helper may
// intentionally start a new session (Python's start_new_session=True is a
// common timeout-management pattern). We therefore freeze the recipe leader,
// discover and freeze all descendants through procfs, then kill both the root
// group and every escaped descendant. Freezing parents before walking their
// children closes the fork-while-cancelling race.
//
// SIGKILL deliberately matches exec.CommandContext's existing cancellation
// semantics; the only change is its scope. Normal completion is unaffected.
func configureToolNodeProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 0 {
			return os.ErrProcessDone
		}
		return killToolNodeProcessTree(cmd.Process.Pid)
	}
}

func killToolNodeProcessTree(rootPID int) error {
	if err := syscall.Kill(rootPID, syscall.SIGSTOP); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}

	descendants := stopToolNodeDescendants(rootPID, map[int]struct{}{rootPID: {}})
	// Kill deepest descendants first. They are stopped, so none can fork or
	// complete and have its PID reused between discovery and this signal.
	for i := len(descendants) - 1; i >= 0; i-- {
		_ = syscall.Kill(descendants[i], syscall.SIGKILL)
	}

	// The root is a process-group leader (Setpgid above). This also catches any
	// same-group process that disappeared from procfs during the walk.
	if err := syscall.Kill(-rootPID, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// Be defensive if a platform/runtime quirk changed the leader's PGID.
			if leaderErr := syscall.Kill(rootPID, syscall.SIGKILL); leaderErr != nil && !errors.Is(leaderErr, syscall.ESRCH) {
				return leaderErr
			}
			return nil
		}
		return err
	}
	return nil
}

// stopToolNodeDescendants returns every descendant of pid after stopping it.
// Linux attributes children to the thread that forked them, so directChildren
// reads every task's `children` file rather than only the thread-group leader.
func stopToolNodeDescendants(pid int, seen map[int]struct{}) []int {
	var out []int
	for _, child := range toolNodeDirectChildren(pid) {
		if child <= 0 {
			continue
		}
		if _, ok := seen[child]; ok {
			continue
		}
		seen[child] = struct{}{}
		if err := syscall.Kill(child, syscall.SIGSTOP); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			// Keep the PID in the kill set on EPERM or another transient error;
			// the later SIGKILL may still succeed.
		}
		out = append(out, child)
		out = append(out, stopToolNodeDescendants(child, seen)...)
	}
	return out
}

func toolNodeDirectChildren(pid int) []int {
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil
	}
	seen := make(map[int]struct{})
	var children []int
	for _, task := range tasks {
		if !task.IsDir() {
			continue
		}
		tid, err := strconv.Atoi(task.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, tid))
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(raw)) {
			child, err := strconv.Atoi(field)
			if err != nil {
				continue
			}
			if _, ok := seen[child]; ok {
				continue
			}
			seen[child] = struct{}{}
			children = append(children, child)
		}
	}
	return children
}
