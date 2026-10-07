//go:build unix

package pisdk

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// awaitSubtreeGone joins the group killSubtree signalled: SIGKILL is
// delivered, not waited, and a still-dying member keeps its open files for a
// moment. Poll the group away so Close does not return while a grandchild of
// the session is still running. The leader itself is already reaped by reap;
// only its orphans can remain. A non-nil onWaitExpired runs once if the
// budget is exhausted with the group still resolvable; it must return
// quickly — this is the Close path, not a place to wait.
func awaitSubtreeGone(pid int, budget time.Duration, onWaitExpired func(pid int, budget time.Duration)) {
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(budget)
	for {
		if err := syscall.Kill(-pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			// The last probe did not answer ESRCH, so the group is still
			// resolvable — a member the SIGKILL cannot finish (zombie pinned
			// to a parent that never reaps, an orphan under a PID-1
			// container) — or the probe itself failed with a non-ESRCH error
			// (EPERM). Both mean "cannot confirm the group is gone": report
			// and leave, the wait budget is spent either way.
			if onWaitExpired != nil {
				onWaitExpired(pid, budget)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// processStartTime reads a process's start-time (field 22 of
// /proc/<pid>/stat), the identity token that distinguishes a recycled pid
// from the process Start spawned. ok=false when procfs cannot answer:
// non-Linux unix, a gone pid, an unreadable stat.
func processStartTime(pid int) (start uint64, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// comm (field 2) is parenthesised and may itself contain spaces or
	// parens, so fields resume after the LAST ')': fields[0] below is
	// field 3 (state), and start-time (field 22) sits at index 19.
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(string(raw[i+1:]))
	if len(fields) <= 19 {
		return 0, false
	}
	start, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, false
	}
	return start, true
}

// pidRecycled reports whether pid now names a process other than the one
// whose start-time was captured. A pid that names nothing cannot head a new
// process group — only this session's orphans can still carry the id — so it
// answers NOT recycled. An existing pid whose start-time cannot be verified
// (procfs missing, nothing captured at Start) answers recycled: the group
// kill is skipped rather than risked on a stranger.
//
// Darwin is covered by the caller's invariants even though processStartTime
// always answers ok=false there: the common path reaps the leader before
// Close reaches the sweep, so kill(pid, 0) returns ESRCH and the sweep
// fires; and when the leader outlives the 5s grace, the pre-existing
// UNGUARDED killSubtree fires while the unreaped leader — alive or zombie —
// still pins its pid, so recycling is impossible on that path.
func pidRecycled(pid int, start uint64, captured bool) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	now, ok := processStartTime(pid)
	if !ok || !captured {
		return true
	}
	return now != start
}
