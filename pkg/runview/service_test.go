package runview

import (
	"context"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// backdateRun ages a seeded run past the orphan grace window so the
// reconcile scan will judge it — the "leftover of a previous process"
// shape the scan exists for.
func backdateRun(t *testing.T, s store.RunStore, id string) {
	t.Helper()
	r, err := s.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("backdate load %s: %v", id, err)
	}
	r.CreatedAt = time.Now().Add(-orphanGraceWindow - time.Minute)
	if err := s.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("backdate save %s: %v", id, err)
	}
}

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

	// The scan only judges runs older than the grace window — backdate the
	// seeds so they look like leftovers of a previous process, which is the
	// scenario this scan exists for.
	for _, id := range []string{"run-orphan-no-cp", "run-orphan-cp", "run-finished", "run-paused"} {
		backdateRun(t, seed, id)
	}

	// run-fresh: status=running but created JUST NOW — the grace window
	// must keep the scan from judging it (regression: a dispatcher-owned
	// run was flipped failed 16s after dispatch by the boot scan while its
	// engine was mid-flight, 2026-07-22 dogfood).
	if _, err := seed.CreateRun(context.Background(), "run-fresh", "wf", nil); err != nil {
		t.Fatalf("create fresh: %v", err)
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
		{"run-fresh", store.RunStatusRunning},
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

// TestCancelInactive_FlipsResumableStatuses verifies that operator
// "cancel" of a paused_waiting_human or failed_resumable run that's
// NOT held by an active goroutine flips the persisted status to
// cancelled. The runtime can then RecoverFinalize on that status so
// the studio's merge UI exposes the partial commits.
func TestCancelInactive_FlipsResumableStatuses(t *testing.T) {
	for _, fromStatus := range []store.RunStatus{
		store.RunStatusPausedWaitingHuman,
		store.RunStatusFailedResumable,
	} {
		t.Run(string(fromStatus), func(t *testing.T) {
			dir := t.TempDir()
			logger := iterlog.Nop()
			seed, err := store.New(dir, store.WithLogger(logger))
			if err != nil {
				t.Fatalf("seed store: %v", err)
			}
			runID := "run-cancel-" + string(fromStatus)
			if _, err := seed.CreateRun(context.Background(), runID, "wf", nil); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := seed.UpdateRunStatus(context.Background(), runID, fromStatus, "setup"); err != nil {
				t.Fatalf("update status: %v", err)
			}
			svc, err := NewService(dir, WithLogger(logger))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			cancelled, err := svc.CancelInactive(runID)
			if err != nil {
				t.Fatalf("CancelInactive: %v", err)
			}
			if !cancelled {
				t.Errorf("CancelInactive returned cancelled=false for %s", fromStatus)
			}
			r, err := seed.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if r.Status != store.RunStatusCancelled {
				t.Errorf("status after cancel = %q, want cancelled", r.Status)
			}
		})
	}
}

// TestCancelInactive_NoOpOnTerminal verifies that calling CancelInactive
// on a run that's ALREADY terminal (finished / failed / cancelled) is a
// no-op — returns (false, nil) and leaves the persisted status alone.
// Important because the HTTP handler dispatches here optimistically when
// manager.Cancel returns ErrRunNotActive, regardless of the run's
// terminal state.
func TestCancelInactive_NoOpOnTerminal(t *testing.T) {
	for _, terminal := range []store.RunStatus{
		store.RunStatusFinished,
		store.RunStatusFailed,
		store.RunStatusCancelled,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			dir := t.TempDir()
			logger := iterlog.Nop()
			seed, err := store.New(dir, store.WithLogger(logger))
			if err != nil {
				t.Fatalf("seed store: %v", err)
			}
			runID := "run-terminal-" + string(terminal)
			if _, err := seed.CreateRun(context.Background(), runID, "wf", nil); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := seed.UpdateRunStatus(context.Background(), runID, terminal, "setup"); err != nil {
				t.Fatalf("update status: %v", err)
			}
			svc, err := NewService(dir, WithLogger(logger))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			cancelled, err := svc.CancelInactive(runID)
			if err != nil {
				t.Fatalf("CancelInactive on terminal returned error: %v", err)
			}
			if cancelled {
				t.Errorf("CancelInactive returned cancelled=true for terminal %s — expected no-op", terminal)
			}
			r, _ := seed.LoadRun(context.Background(), runID)
			if r.Status != terminal {
				t.Errorf("status mutated from %q to %q — expected no-op", terminal, r.Status)
			}
		})
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
