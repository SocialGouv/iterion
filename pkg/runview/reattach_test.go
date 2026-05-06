package runview

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestReconcileOrphans_ReattachesLiveDetachedRunner verifies the new
// PID-aware reconcile path: a run with status=running and a .pid
// pointing at a live process should be re-attached (manager handle
// installed, file tailers wired) rather than flipped to
// failed_resumable.
//
// We use `sleep 30` as the stand-in runner — it has a stable PID we
// can write to the .pid file, and we kill it explicitly at the end of
// the test so the watcher's exit-detection path also gets exercised.
func TestReconcileOrphans_ReattachesLiveDetachedRunner(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()

	// Seed: a "running" run with a .pid pointing at a live sleep.
	seed, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	const id = "run-detached-live"
	if _, err := seed.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Status is already "running" by default after CreateRun.

	cmd := exec.Command("sleep", "30")
	// Detach the test's sleep into its own session so our SIGTERM-to-pgrp
	// in cancel() finds something to signal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap the child as soon as it exits so kill(pid, 0) eventually
	// returns ESRCH instead of leaving a zombie indefinitely. In
	// production the runner is fully detached (Setsid + double-fork
	// equivalent) and reparented to init, which reaps automatically;
	// in this test we are still the parent, so we have to do it.
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-reaped
	})

	if err := seed.WritePIDFile(id, pid); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}

	// Constructing the service runs reconcileOrphans synchronously.
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if !svc.manager.Active(id) {
		t.Fatalf("expected run %q to be re-attached, but Active=false", id)
	}

	// Status must remain "running" — the legacy reconcile path would
	// have flipped it.
	r, err := svc.store.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusRunning {
		t.Errorf("status = %q, want running (re-attach must NOT flip status)", r.Status)
	}

	// Now kill the sleep and verify the watcher detects the exit and
	// Deregisters the handle within a reasonable budget. The watcher
	// polls at 1s cadence so 4s gives 3 ticks of slack.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		t.Fatalf("kill sleep: %v", err)
	}

	// Watcher polls at 5s cadence, plus reaping latency on Linux —
	// 10s gives one tick of slack.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !svc.manager.Active(id) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if svc.manager.Active(id) {
		t.Errorf("manager still tracks %q after runner exit; watcher did not Deregister", id)
	}
}

// TestReconcileOrphans_StalePIDFlipsStatus verifies the cleanup path:
// a run with status=running + a .pid that points at a dead PID should
// be flipped to failed_resumable, AND the stale .pid file removed.
func TestReconcileOrphans_StalePIDFlipsStatus(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()

	seed, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	const id = "run-stale-pid"
	if _, err := seed.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Save a checkpoint so the resulting status is failed_resumable
	// (otherwise the orphan path lands on plain failed).
	if err := seed.SaveCheckpoint(context.Background(), id, &store.Checkpoint{NodeID: "n1"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	// Spawn + immediately kill a child to obtain a PID we know is
	// dead. PID collisions during the test window are vanishingly
	// rare; using a freshly-reaped PID rather than picking a number
	// avoids accidentally signalling someone else's process.
	cmd := exec.CommandContext(context.Background(), "true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn true: %v", err)
	}
	deadPID := cmd.ProcessState.Pid()
	if deadPID <= 0 {
		t.Skipf("could not extract a known-dead PID (got %d)", deadPID)
	}
	if err := seed.WritePIDFile(id, deadPID); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}

	if _, err := NewService(dir, WithLogger(logger)); err != nil {
		t.Fatalf("NewService: %v", err)
	}

	verify, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("verify store: %v", err)
	}
	r, err := verify.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %q, want failed_resumable", r.Status)
	}

	if pid, err := verify.ReadPIDFile(id); err != nil || pid != 0 {
		t.Errorf("ReadPIDFile after stale reconcile = (%d, %v), want (0, nil) — stale .pid not cleaned up", pid, err)
	}
}
