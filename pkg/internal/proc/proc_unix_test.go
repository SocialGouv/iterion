//go:build unix

package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// heartbeatRecipe backgrounds a loop that appends to path every 100ms and
// then blocks on `wait`. The loop is the GRANDCHILD — a child of the shell,
// reparented when the shell dies — so the file is an oracle for the subtree,
// not for the direct child. Bounded at ~20s so a regression leaves nothing
// spinning on the host.
func heartbeatRecipe(path string) string {
	return `( i=0; while [ "$i" -lt 200 ]; do printf 'tick\n' >> "` + path + `"; ` +
		`i=$((i+1)); sleep 0.1; done ) & wait`
}

func size(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func waitForFirstTick(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if size(path) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no heartbeat at %s — the recipe never started", path)
}

// TestTerminateGroupOnCancelReachesTheGrandchild pins the primitive: after
// cancellation the whole subtree is gone, not just the shell we spawned.
func TestTerminateGroupOnCancelReachesTheGrandchild(t *testing.T) {
	heart := filepath.Join(t.TempDir(), "heartbeat")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", heartbeatRecipe(heart))
	TerminateGroupOnCancel(cmd)

	done := make(chan error, 1)
	go func() {
		_, err := cmd.Output()
		done <- err
	}()

	waitForFirstTick(t, heart)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Output did not return within 5s of cancellation — a descendant still holds the stdout pipe")
	}

	time.Sleep(300 * time.Millisecond)
	before := size(heart)
	time.Sleep(600 * time.Millisecond)
	if after := size(heart); after != before {
		t.Fatalf("the grandchild survived cancellation: heartbeat grew %d -> %d bytes", before, after)
	}
}

// TestTerminateGroupOnCancelLeavesASuccessfulRunAlone is the negative case:
// the primitive must not touch a command that completes on its own — same
// stdout, same exit status, no group signal in sight.
func TestTerminateGroupOnCancelLeavesASuccessfulRunAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "printf 'ok'")
	TerminateGroupOnCancel(cmd)

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if string(out) != "ok" {
		t.Fatalf("stdout = %q, want %q", out, "ok")
	}
}

// TestTerminateGroupOnCancelOnAReapedCommandIsNotAnError covers the ESRCH
// arm. os/exec's watchCtx can call Cancel after Wait already reaped the
// process (the two are a select race), and there the group is gone: mapping
// ESRCH to os.ErrProcessDone is what keeps that race from replacing a
// command's real exit status with "canceling Cmd: no such process".
//
// Reaping is the precondition, not exiting: until Wait runs the child is a
// zombie, still a member of its group, and kill(-pid) succeeds.
func TestTerminateGroupOnCancelOnAReapedCommandIsNotAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "printf 'ok'")
	TerminateGroupOnCancel(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := syscall.Kill(-pid, 0); err != syscall.ESRCH {
		t.Fatalf("group %d still addressable after reaping (%v) — the precondition this test needs does not hold", pid, err)
	}

	if err := cmd.Cancel(); err != os.ErrProcessDone {
		t.Fatalf("Cancel on a reaped group = %v, want os.ErrProcessDone", err)
	}
}

// TestDetachProcessGroupDoesNotSignal keeps the two primitives distinct: the
// isolation-only one must never install a Cancel, or every caller that picked
// it to SURVIVE a parent's signal would start dying with its context.
func TestDetachProcessGroupDoesNotSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "true")
	DetachProcessGroup(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("DetachProcessGroup did not set Setpgid")
	}
	// exec.CommandContext installs its own default Cancel (kill the direct
	// child); what must not appear is a group signal on top of it.
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
