//go:build unix

package pisdk

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

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
