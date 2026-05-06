package runview

import (
	"context"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestReconcileOrphans seeds a store with a mix of run statuses,
// constructs a Service, and verifies that only "running" rows whose
// lock is currently free get flipped to a terminal status (and that
// the right terminal status is chosen based on checkpoint presence).
func TestReconcileOrphans(t *testing.T) {
	dir := t.TempDir()

	// Seed runs through a separate store handle, mimicking what a
	// previous CLI invocation would leave behind.
	logger := iterlog.Nop()
	seed, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}

	// run-orphan-no-cp: status=running, no checkpoint → expect failed
	if _, err := seed.CreateRun(context.Background(), "run-orphan-no-cp", "wf", nil); err != nil {
		t.Fatalf("create no-cp: %v", err)
	}

	// run-orphan-cp: status=running, with checkpoint → expect failed_resumable
	if _, err := seed.CreateRun(context.Background(), "run-orphan-cp", "wf", nil); err != nil {
		t.Fatalf("create cp: %v", err)
	}
	if err := seed.SaveCheckpoint(context.Background(), "run-orphan-cp", &store.Checkpoint{NodeID: "n1"}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	// run-finished: should be untouched
	if _, err := seed.CreateRun(context.Background(), "run-finished", "wf", nil); err != nil {
		t.Fatalf("create finished: %v", err)
	}
	if err := seed.UpdateRunStatus(context.Background(), "run-finished", store.RunStatusFinished, ""); err != nil {
		t.Fatalf("update finished: %v", err)
	}

	// run-paused: paused_waiting_human, should also be untouched
	if _, err := seed.CreateRun(context.Background(), "run-paused", "wf", nil); err != nil {
		t.Fatalf("create paused: %v", err)
	}
	if err := seed.PauseRun(context.Background(), "run-paused", &store.Checkpoint{NodeID: "n1"}); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Now construct the service — reconcileOrphans runs in NewService.
	if _, err := NewService(dir, WithLogger(logger)); err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Verify outcomes via a fresh store handle.
	verify, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("verify store: %v", err)
	}

	cases := []struct {
		id   string
		want store.RunStatus
	}{
		{"run-orphan-no-cp", store.RunStatusFailed},
		{"run-orphan-cp", store.RunStatusFailedResumable},
		{"run-finished", store.RunStatusFinished},
		{"run-paused", store.RunStatusPausedWaitingHuman},
	}
	for _, c := range cases {
		r, err := verify.LoadRun(context.Background(), c.id)
		if err != nil {
			t.Errorf("LoadRun %s: %v", c.id, err)
			continue
		}
		if r.Status != c.want {
			t.Errorf("%s: status = %q, want %q", c.id, r.Status, c.want)
		}
	}
}

// TestReconcileOrphans_LiveProcessLeftAlone verifies that a "running"
// run held by a live lock is NOT clobbered. Mimics two iterion
// processes sharing a store dir.
func TestReconcileOrphans_LiveProcessLeftAlone(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()

	seed, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if _, err := seed.CreateRun(context.Background(), "run-live", "wf", nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	// "Process A" holds the lock — keep it open through the test.
	lock, err := seed.LockRun(context.Background(), "run-live")
	if err != nil {
		t.Fatalf("LockRun: %v", err)
	}
	defer func() { _ = lock.Unlock() }()

	// "Process B" starts up.
	if _, err := NewService(dir, WithLogger(logger)); err != nil {
		t.Fatalf("NewService: %v", err)
	}

	r, err := seed.LoadRun(context.Background(), "run-live")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusRunning {
		t.Errorf("status = %q, want unchanged 'running' (live process holds lock)", r.Status)
	}
}
