package runview

import (
	"context"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestReconcileOrphans seeds a store with a mix of run statuses,
// constructs a Service, and verifies that only "running" rows whose
// lock is currently free get flipped to failed_resumable, whether recovery
// continues from a checkpoint or restarts from entry.
func TestReconcileOrphans(t *testing.T) {
	dir := t.TempDir()

	// Seed runs through a separate store handle, mimicking what a
	// previous CLI invocation would leave behind.
	logger := iterlog.Nop()
	seed, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}

	// run-orphan-no-cp: status=running, no checkpoint → resumable from entry
	if _, err := seed.CreateRun(context.Background(), "run-orphan-no-cp", "wf", nil); err != nil {
		t.Fatalf("create no-cp: %v", err)
	}

	// run-orphan-cp: status=running, with checkpoint → resumable from checkpoint
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
		{"run-orphan-no-cp", store.RunStatusFailedResumable},
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

	for _, id := range []string{"run-orphan-no-cp", "run-orphan-cp"} {
		events, err := verify.LoadEvents(context.Background(), id)
		if err != nil {
			t.Fatalf("LoadEvents %s: %v", id, err)
		}
		if len(events) != 1 || events[0].Type != store.EventRunInterrupted {
			t.Errorf("%s: events = %#v, want one run_interrupted lifecycle event", id, events)
		}
	}
}

// TestReconcileOrphansBootCleansNestedTree ensures ParentRunID alone never
// masquerades as liveness. At service startup no Manager handle exists yet, so
// a stale root and its stale child from the previous process are both orphans.
func TestReconcileOrphansBootCleansNestedTree(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	seed, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	const (
		rootID  = "run-boot-orphan-root"
		childID = "run-boot-orphan-child"
	)
	if _, err := seed.CreateRun(context.Background(), rootID, "wf", nil); err != nil {
		t.Fatalf("CreateRun(root): %v", err)
	}
	child, err := seed.CreateRun(context.Background(), childID, "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun(child): %v", err)
	}
	child.ParentRunID = rootID
	if err := seed.SaveRun(context.Background(), child); err != nil {
		t.Fatalf("SaveRun(child): %v", err)
	}

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Stop(context.Background())

	for _, id := range []string{rootID, childID} {
		r, loadErr := svc.store.LoadRun(context.Background(), id)
		if loadErr != nil {
			t.Fatalf("LoadRun(%s): %v", id, loadErr)
		}
		if r.Status != store.RunStatusFailedResumable {
			t.Fatalf("%s status after boot reconcile = %q, want failed_resumable", id, r.Status)
		}
	}
}

func TestReconcileRunRecordsInterruptAndRestartsFromEntry(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const id = "run-on-demand-orphan"
	if _, err := st.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Construct directly so the boot reconcile does not consume the fixture;
	// this pins the resume-request/on-demand path specifically.
	svc := &Service{store: st, manager: NewManager(), logger: logger}
	r, reconciled, err := svc.reconcileRun(id)
	if err != nil {
		t.Fatalf("reconcileRun: %v", err)
	}
	if !reconciled {
		t.Fatal("reconcileRun reported reconciled=false for an unlocked running run")
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %q, want failed_resumable", r.Status)
	}
	if r.Checkpoint != nil {
		t.Fatalf("checkpoint = %#v, want nil so resume restarts from entry", r.Checkpoint)
	}
	events, err := st.LoadEvents(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 1 || events[0].Type != store.EventRunInterrupted {
		t.Fatalf("events = %#v, want one run_interrupted", events)
	}
}

func TestReconcileRunPreservesNestedRunUnderActiveAncestor(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	const (
		rootID  = "run-on-demand-active-root"
		childID = "run-on-demand-live-child"
	)
	if _, err := st.CreateRun(context.Background(), rootID, "wf", nil); err != nil {
		t.Fatalf("CreateRun(root): %v", err)
	}
	child, err := st.CreateRun(context.Background(), childID, "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun(child): %v", err)
	}
	child.ParentRunID = rootID
	child.ParentNodeID = "child_subbot"
	if err := st.SaveRun(context.Background(), child); err != nil {
		t.Fatalf("SaveRun(child): %v", err)
	}

	svc := &Service{store: st, manager: NewManager(), logger: logger}
	if _, err := svc.manager.Register(context.Background(), rootID); err != nil {
		t.Fatalf("register active root: %v", err)
	}

	r, reconciled, err := svc.reconcileRun(childID)
	if err != nil {
		t.Fatalf("reconcileRun(child): %v", err)
	}
	if reconciled {
		t.Fatal("active nested child was reported as reconciled")
	}
	if r.Status != store.RunStatusRunning {
		t.Fatalf("child status = %q, want running", r.Status)
	}

	svc.manager.Deregister(rootID)
	r, reconciled, err = svc.reconcileRun(childID)
	if err != nil {
		t.Fatalf("reconcileRun(orphan child): %v", err)
	}
	if !reconciled || r.Status != store.RunStatusFailedResumable {
		t.Fatalf("after root deregister: reconciled=%v status=%q, want true/failed_resumable", reconciled, r.Status)
	}
}

// TestReconcileRunDoesNotInheritLivenessForNonSubbotChild ensures a shard or
// fork cannot hide behind a live parent. Those children execute independently
// and therefore have ParentRunID but no ParentNodeID; losing their own lock is
// a real orphan even while the parent remains active.
func TestReconcileRunDoesNotInheritLivenessForNonSubbotChild(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const (
		rootID  = "run-active-parent"
		childID = "run-orphan-async-child"
	)
	if _, err := st.CreateRun(context.Background(), rootID, "wf", nil); err != nil {
		t.Fatalf("CreateRun(root): %v", err)
	}
	child, err := st.CreateRun(context.Background(), childID, "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun(child): %v", err)
	}
	child.ParentRunID = rootID
	if err := st.SaveRun(context.Background(), child); err != nil {
		t.Fatalf("SaveRun(child): %v", err)
	}

	svc := &Service{store: st, manager: NewManager(), logger: logger}
	if _, err := svc.manager.Register(context.Background(), rootID); err != nil {
		t.Fatalf("register active root: %v", err)
	}
	defer svc.manager.Deregister(rootID)

	r, reconciled, err := svc.reconcileRun(childID)
	if err != nil {
		t.Fatalf("reconcileRun(child): %v", err)
	}
	if !reconciled || r.Status != store.RunStatusFailedResumable {
		t.Fatalf("reconciled=%v status=%q, want true/failed_resumable", reconciled, r.Status)
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

func TestCancelInactive_RefusesHeldRunLock(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "run-paused-lock-held"
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.PauseRun(context.Background(), runID, &store.Checkpoint{NodeID: "gate"}); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}
	lock, err := st.LockRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LockRun: %v", err)
	}
	defer func() { _ = lock.Unlock() }()

	svc := &Service{store: st, manager: NewManager(), logger: logger}
	cancelled, err := svc.CancelInactive(runID)
	if err == nil || cancelled {
		t.Fatalf("CancelInactive while locked: cancelled=%v err=%v, want false/error", cancelled, err)
	}
	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("locked run status=%q, want paused_waiting_human", r.Status)
	}
}

func TestControlMethodsDoNotCreateGhostRuns(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func(*Service) error
	}{
		{
			name: "cancel-inactive",
			call: func(svc *Service) error {
				_, err := svc.CancelInactiveCtx(ctx, "run-does-not-exist")
				return err
			},
		},
		{
			name: "commit-and-finalize",
			call: func(svc *Service) error {
				_, err := svc.CommitAndFinalizeCtx(ctx, "run-does-not-exist", "feat: impossible")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.New(t.TempDir(), store.WithLogger(iterlog.Nop()))
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			svc := &Service{store: st, manager: NewManager(), logger: iterlog.Nop()}
			if err := tc.call(svc); err == nil {
				t.Fatal("unknown run was accepted")
			}
			ids, err := st.ListRuns(ctx)
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(ids) != 0 {
				t.Fatalf("unknown-run control created ghost run directories: %v", ids)
			}
		})
	}
}

func TestCancelInactive_AllowsPausedSubbotUnderActiveParent(t *testing.T) {
	ctx := context.Background()
	logger := iterlog.Nop()
	st, err := store.New(t.TempDir(), store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const (
		parentID = "run-active-parent-waiting"
		childID  = "run-paused-subbot"
	)
	if _, err := st.CreateRun(ctx, parentID, "parent", nil); err != nil {
		t.Fatalf("CreateRun(parent): %v", err)
	}
	child, err := st.CreateRun(ctx, childID, "child", nil)
	if err != nil {
		t.Fatalf("CreateRun(child): %v", err)
	}
	child.ParentRunID = parentID
	child.ParentNodeID = "dispatch_child"
	child.Status = store.RunStatusPausedWaitingHuman
	if err := st.SaveRun(ctx, child); err != nil {
		t.Fatalf("SaveRun(child): %v", err)
	}

	svc := &Service{store: st, manager: NewManager(), logger: logger}
	if _, err := svc.manager.Register(ctx, parentID); err != nil {
		t.Fatalf("register active parent: %v", err)
	}
	defer svc.manager.Deregister(parentID)

	cancelled, err := svc.CancelInactiveCtx(ctx, childID)
	if err != nil {
		t.Fatalf("CancelInactiveCtx(paused child under active parent): %v", err)
	}
	if !cancelled {
		t.Fatal("CancelInactiveCtx returned cancelled=false for the paused child")
	}
	got, err := st.LoadRun(ctx, childID)
	if err != nil {
		t.Fatalf("LoadRun(child): %v", err)
	}
	if got.Status != store.RunStatusCancelled {
		t.Fatalf("child status=%q, want cancelled", got.Status)
	}
}

func TestCancelInactive_RefusesDirectManagerOwner(t *testing.T) {
	ctx := context.Background()
	logger := iterlog.Nop()
	st, err := store.New(t.TempDir(), store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	const runID = "run-paused-direct-owner"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.PauseRun(ctx, runID, &store.Checkpoint{NodeID: "gate"}); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}
	svc := &Service{store: st, manager: NewManager(), logger: logger}
	if _, err := svc.manager.Register(ctx, runID); err != nil {
		t.Fatalf("register active run: %v", err)
	}
	defer svc.manager.Deregister(runID)

	cancelled, err := svc.CancelInactiveCtx(ctx, runID)
	if err == nil || cancelled {
		t.Fatalf("CancelInactiveCtx direct owner: cancelled=%v err=%v, want false/error", cancelled, err)
	}
}

func TestCommitAndFinalize_RejectsRunningOrLockedRun(t *testing.T) {
	ctx := context.Background()
	logger := iterlog.Nop()

	t.Run("running", func(t *testing.T) {
		st, err := store.New(t.TempDir(), store.WithLogger(logger))
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		const runID = "run-commit-finalize-running"
		if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		svc := &Service{store: st, manager: NewManager(), logger: logger}
		if _, err := svc.CommitAndFinalizeCtx(ctx, runID, "feat: unsafe"); err == nil {
			t.Fatal("CommitAndFinalizeCtx accepted a running run")
		}
	})

	t.Run("lock-held", func(t *testing.T) {
		st, err := store.New(t.TempDir(), store.WithLogger(logger))
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		const runID = "run-commit-finalize-locked"
		if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if err := st.UpdateRunStatus(ctx, runID, store.RunStatusFinished, ""); err != nil {
			t.Fatalf("UpdateRunStatus: %v", err)
		}
		lock, err := st.LockRun(ctx, runID)
		if err != nil {
			t.Fatalf("LockRun: %v", err)
		}
		defer func() { _ = lock.Unlock() }()

		svc := &Service{store: st, manager: NewManager(), logger: logger}
		if _, err := svc.CommitAndFinalizeCtx(ctx, runID, "feat: unsafe"); err == nil {
			t.Fatal("CommitAndFinalizeCtx accepted a held run lock")
		}
	})
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
