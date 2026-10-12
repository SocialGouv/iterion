//go:build unix

package pisdk

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// spawnGroupLeader starts a real process leading its own process group —
// the shape hardenSubtreeTermination gives pi.
func spawnGroupLeader(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep: %v", err)
	}
	return cmd
}

func TestAwaitSubtreeGoneSignalsWaitExpired(t *testing.T) {
	cmd := spawnGroupLeader(t)
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	// A group still alive at budget expiry is the pinned case the hook
	// exists for: zombie-pinned members keep kill(-pgid, 0) answering
	// alive exactly like this, so ESRCH never comes and the poll burns
	// its whole budget. The hook must fire, once, with the pid and the
	// budget it starved on.
	var gotPid []int
	var gotBudget []time.Duration
	awaitSubtreeGone(pid, 10*time.Millisecond, func(p int, budget time.Duration) {
		gotPid = append(gotPid, p)
		gotBudget = append(gotBudget, budget)
	})
	if len(gotPid) != 1 || gotPid[0] != pid {
		t.Fatalf("wait-expired hook called %d time(s) with pids %v (want exactly once with pid %d)", len(gotPid), gotPid, pid)
	}
	if len(gotBudget) != 1 || gotBudget[0] != 10*time.Millisecond {
		t.Fatalf("wait-expired hook got budgets %v (want exactly once with 10ms)", gotBudget)
	}

	// The zero value keeps the historical silence: a nil hook must ride the
	// same expiry without panicking.
	awaitSubtreeGone(pid, 10*time.Millisecond, nil)
}

func TestAwaitSubtreeGoneSilentWhenGroupGone(t *testing.T) {
	cmd := spawnGroupLeader(t)
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = cmd.Wait() // reaped: the group id names nothing now
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Skip("group id still resolvable right after reap")
	}

	// The success path must stay silent: the hook reports a PINNED group,
	// never a sweep that finished its job.
	fired := false
	awaitSubtreeGone(pid, 10*time.Millisecond, func(int, time.Duration) { fired = true })
	if fired {
		t.Error("wait-expired hook fired on a group that reached ESRCH — the success path must never fire it")
	}
}

func TestProcessStartTime(t *testing.T) {
	start, ok := processStartTime(os.Getpid())
	if !ok {
		t.Skip("procfs unavailable on this platform")
	}
	if start == 0 {
		t.Fatal("start-time = 0 — the field-22 parse drifted")
	}
	again, ok := processStartTime(os.Getpid())
	if !ok || again != start {
		t.Fatalf("start-time unstable: %d then %d (ok=%v)", start, again, ok)
	}
	if _, ok := processStartTime(1 << 30); ok {
		t.Error("an implausible pid answered ok — stat read must fail")
	}
}

func TestPidRecycled(t *testing.T) {
	self, ok := processStartTime(os.Getpid())
	if !ok {
		t.Skip("procfs unavailable on this platform")
	}

	// The pid still names the captured process: not recycled, the sweep's
	// kill remains addressed at our own group.
	if pidRecycled(os.Getpid(), self, true) {
		t.Error("self with its own start-time reported recycled")
	}
	// Same pid, a different start-time: reused by another process — the
	// group kill must be skipped.
	if !pidRecycled(os.Getpid(), self+1, true) {
		t.Error("same pid with a different start-time not reported recycled")
	}
	// Nothing captured at Start: recycling cannot be ruled out — skip.
	if !pidRecycled(os.Getpid(), self, false) {
		t.Error("no captured start-time must answer recycled")
	}

	// A reaped child's pid names nothing: no new group can hold the id, so
	// NOT recycled — the sweep can only reach this session's orphans, which
	// is the case it exists for.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Skip("pid still allocated right after reap")
	}
	if pidRecycled(pid, self, true) {
		t.Error("a gone pid reported recycled — the sweep would skip the orphans it exists to kill")
	}
}
