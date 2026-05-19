package runview

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	dockersandbox "github.com/SocialGouv/iterion/pkg/sandbox/docker"
	"github.com/SocialGouv/iterion/pkg/store"
)

// reconcileSandboxContainers force-removes managed docker/podman
// containers whose run has reached a terminal status (or vanished from
// the store entirely). Without this, a daemon SIGTERM mid-run leaves
// the container up (--rm only fires on graceful exit) and the next
// boot of the same run trips on container-name conflict — or worse,
// the operator accumulates orphan sandboxes consuming RAM until
// `docker ps -a` is manually pruned.
//
// Safe to call when docker/podman isn't installed: dockersandbox.Detect
// returns an error which we swallow as "nothing to reconcile."
func (s *Service) reconcileSandboxContainers() {
	rt, err := dockersandbox.Detect()
	if err != nil {
		return
	}
	// Boot-time admin scan: peek at runs across tenants to decide
	// whether their docker leftovers should be reaped.
	ctx := store.WithoutTenantFilter(context.Background())
	reaped, err := dockersandbox.ReapOrphanContainers(ctx, rt, func(runID string) bool {
		if runID == "" {
			return true
		}
		r, loadErr := s.store.LoadRun(ctx, runID)
		if loadErr != nil {
			return true
		}
		switch r.Status {
		case store.RunStatusRunning, store.RunStatusPausedWaitingHuman:
			return false
		default:
			return true
		}
	})
	if err != nil {
		s.logger.Warn("runview: reap orphan containers: %v", err)
	}
	if len(reaped) > 0 {
		s.logger.Info("runview: reaped %d orphan sandbox container(s)", len(reaped))
	}
}

// reconcileOrphans flips runs whose status is "running" but whose
// owning process is gone (lock released by the OS) to a terminal
// status. Without this, every server restart leaves the studio's
// run list polluted with stale "running" rows from CLI invocations
// that exited (cleanly or otherwise) without persisting a final
// status — flock(2) is auto-released on crash, but the engine's
// status writer is not.
//
// Logic per orphan:
//   - has Checkpoint  → failed_resumable (user can iterion resume)
//   - no Checkpoint   → failed           (no recovery point; restart)
//
// We use the lock as the liveness probe: a non-blocking flock that
// succeeds proves no other process holds the run. Held runs are left
// untouched, so a second iterion instance running in the same store
// dir cannot clobber the first instance's in-flight work.
func (s *Service) reconcileOrphans() {
	// Boot-time admin scan: no JWT, no tenant on the request. Tag the
	// ctx so the mongo store's tenant guard allows the cross-tenant
	// ListRuns / LoadRun / UpdateRunStatus calls that follow. The
	// filesystem store ignores the flag (no tenant scoping there).
	ctx := store.WithoutTenantFilter(context.Background())
	ids, err := s.store.ListRuns(ctx)
	if err != nil {
		s.logger.Warn("runview: reconcile: list runs: %v", err)
		return
	}
	for _, id := range ids {
		r, err := s.store.LoadRun(ctx, id)
		if err != nil {
			continue
		}
		// Recover missed finalization for worktree runs whose daemon
		// died between "Run finished" and finalizeWorktree completing.
		// The recovery is idempotent (bails when FinalBranch is set or
		// the run isn't a finished-worktree case), so it's safe to call
		// for every run scanned. Without this, a SIGTERM landing during
		// the ~50ms window between status=finished and SaveRun(final_*)
		// leaves the run forever stuck with no merge UI affordance.
		if recErr := runtime.RecoverFinalize(ctx, s.store, r, s.logger); recErr != nil {
			s.logger.Warn("runview: recover finalize %s: %v", id, recErr)
		}
		if r.Status != store.RunStatusRunning {
			continue
		}
		// .pid present + PID alive → runner outlived the previous
		// server lifetime; re-attach. Stale .pid → remove and fall
		// through to the flock probe. Missing .pid → in-process or
		// older run; flock probe applies.
		if s.tryReattachByPID(id) {
			continue
		}
		// Try to grab the lock; non-blocking semantics mean we
		// either own it instantly (orphan) or fail fast (live).
		lock, err := s.store.LockRun(ctx, id)
		if err != nil {
			continue
		}
		// Re-load under the lock — another process could have
		// just released between ListRuns and LockRun and updated
		// the status to a terminal state.
		r2, err := s.store.LoadRun(ctx, id)
		if err != nil || r2.Status != store.RunStatusRunning {
			_ = lock.Unlock()
			continue
		}
		newStatus := store.RunStatusFailed
		if r2.Checkpoint != nil {
			newStatus = store.RunStatusFailedResumable
		}
		if err := s.store.UpdateRunStatus(ctx, id, newStatus, "process orphaned: server restart found run in 'running' state"); err != nil {
			s.logger.Warn("runview: reconcile %s: %v", id, err)
		} else {
			s.logger.Info("runview: reconciled orphan run %s → %s", id, newStatus)
		}
		_ = lock.Unlock()
	}
}

// tryReattachByPID handles the .pid path of reconcileOrphans. Returns
// true if the run was re-attached (caller should skip the orphan
// reconcile). Removes a stale .pid as a side effect so the next
// reconcile cycle doesn't false-positive on it.
func (s *Service) tryReattachByPID(runID string) bool {
	pidS := store.AsPIDStore(s.store)
	if pidS == nil {
		return false
	}
	pid, err := pidS.ReadPIDFile(runID)
	if err != nil || pid <= 0 {
		return false
	}
	if pidAlive(pid) == nil {
		s.reattachDetached(runID, pid)
		return true
	}
	_ = pidS.RemovePIDFile(runID)
	return false
}

// reattachDetached re-establishes the studio server's view of a
// detached runner that survived a previous server lifetime. It
// installs an in-memory log buffer (so WS subscribers can stream
// live), starts the file-based event + log tailers, and registers a
// manager handle whose Cancel signals the runner's process group and
// whose done channel is closed by a watcher goroutine that polls for
// process exit.
//
// We can't cmd.Wait() on the runner here — we are not its parent —
// so liveness is inferred via kill -0 polling at 1s cadence. That
// resolution is fine: the runner can take seconds to reach its
// shutdown checkpoints anyway, and the watcher's only consumer is
// Drain (timing-sensitive) and the broker.CloseRun call (post-mortem).
func (s *Service) reattachDetached(runID string, pid int) {
	s.prepareRunLogNoFile(runID)

	done := make(chan struct{})
	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			if err := terminateProcessGroup(pid); err != nil {
				s.logger.Warn("runview: reattach: signal pgrp %d: %v", pid, err)
			}
		})
	}

	if err := s.manager.RegisterDetached(runID, pid, cancel, done); err != nil {
		s.logger.Warn("runview: reattach: register %s pid=%d: %v", runID, pid, err)
		return
	}

	go func() {
		watchDetachedExit(s, runID, pid, done)
	}()

	startEventSource(s, runID, done)
	startLogSource(s, runID, done)

	s.logger.Info("runview: re-attached detached run %s (pid=%d) across server restart", runID, pid)
}

// watchDetachedExit polls kill(0) on pid until the process exits,
// then performs the same cleanup spawnDetached's cmd.Wait goroutine
// would: clean up the .pid file, close subscriptions, and Deregister
// the handle (which closes done). Used only on the re-attach path
// where we don't own the cmd. 5s cadence is fine because runners
// typically run for minutes; a faster probe would just burn syscalls.
func watchDetachedExit(s *Service, runID string, pid int, done chan struct{}) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := pidAlive(pid); err != nil {
				if pidS := store.AsPIDStore(s.store); pidS != nil {
					_ = pidS.RemovePIDFile(runID)
				}
				s.broker.CloseRun(runID)
				s.dropRunLog(runID)
				s.manager.Deregister(runID)
				return
			}
		}
	}
}

// Stop cancels every active run and waits for their goroutines to
// finish, but does not flip persisted statuses or emit any
// observability event. Use Stop in tests or for a quiet teardown
// where the caller takes responsibility for the on-disk state.
//
// Production shutdown should call Drain instead, which additionally
// publishes EventRunInterrupted and flips each in-flight run to
// failed_resumable so the next server boot can offer one-click resume.
func (s *Service) Stop(ctx context.Context) {
	s.manager.Stop(ctx)
}

// Drain performs a graceful shutdown of every active run:
//
//  1. Sets the draining flag so subsequent Launch / Resume return
//     runtime.ErrServerDraining.
//  2. Snapshots active handles and cancels each one.
//  3. Waits on each handle's done channel up to ctx's deadline.
//  4. For every run that was active at the moment of Drain — whether
//     its goroutine exited cleanly within the deadline or not —
//     emits EventRunInterrupted and flips the persisted status to
//     failed_resumable with reason "server drained".
//
// The status flip happens regardless of clean exit so the on-disk
// state is unambiguous; the runtime's own failure event (typically
// EventRunFailed with cause "context canceled") may also land in
// the same events.jsonl, which is acceptable telemetry noise — both
// events accurately describe what happened.
//
// Drain is intended to be called once during process shutdown. After
// it returns, the service should not be used to launch new work.
func (s *Service) Drain(ctx context.Context) {
	s.draining.Store(true)

	handles := s.manager.Snapshot()

	for _, h := range handles {
		h.Cancel()
	}

	for _, h := range handles {
		select {
		case <-h.Done:
		case <-ctx.Done():
			// Out of time — record what's still live then bail out.
			s.markRemainingInterrupted(handles)
			return
		}
	}

	// All goroutines drained within budget. Flip statuses + emit events.
	for _, h := range handles {
		s.markInterrupted(h.RunID)
	}
}

// markRemainingInterrupted marks every snapshot handle as interrupted.
// Used on the deadline-exceeded path where we can't tell which
// individual handles are still live without re-snapshotting; flipping
// all of them is idempotent (UpdateRunStatus tolerates the run already
// being in a terminal state — it just rewrites the status).
func (s *Service) markRemainingInterrupted(handles []HandleSnapshot) {
	for _, h := range handles {
		s.markInterrupted(h.RunID)
	}
}

// markInterrupted emits EventRunInterrupted and flips the run's status
// to failed_resumable with reason "server drained". Errors are logged
// at warn level — drain must not abort over a single run's bookkeeping.
//
// Drain is a system-level operation that writes housekeeping events
// for runs the server itself owns at shutdown; the handle snapshot
// does not carry per-run tenant_id, so we use WithoutTenantFilter to
// bypass the mongo backend's fail-closed guard. Without this the
// drain panics in cloud mode the moment any active run exists.
func (s *Service) markInterrupted(runID string) {
	const reason = "server drained: studio process shutting down"
	ctx := store.WithoutTenantFilter(context.Background())
	if _, err := s.store.AppendEvent(ctx, runID, store.Event{
		Type:  store.EventRunInterrupted,
		RunID: runID,
		Data:  map[string]interface{}{"reason": reason},
	}); err != nil {
		s.logger.Warn("runview: drain: append run_interrupted for %s: %v", runID, err)
	}
	if err := s.store.UpdateRunStatus(ctx, runID, store.RunStatusFailedResumable, reason); err != nil {
		s.logger.Warn("runview: drain: update status for %s: %v", runID, err)
	}
}

// reconcileRun is the on-demand counterpart to reconcileOrphans: when a
// resume request arrives for a run still flagged `running` and the
// service has no active handle for it, the run is an orphan from a
// previous server lifetime (or a goroutine that died abruptly). Trying
// to grab the lock — which the OS auto-releases on process exit — proves
// liveness; if it succeeds, the run is genuinely dead and we flip the
// status so resume can proceed. If the lock is held (live goroutine in
// this process or another), nothing happens and resume rejects normally.
//
// Returns the up-to-date run (post-reconcile if it fired) so the caller
// doesn't have to re-load.
func (s *Service) reconcileRun(runID string) (*store.Run, bool, error) {
	r, err := s.store.LoadRun(context.Background(), runID)
	if err != nil {
		return nil, false, err
	}
	if r.Status != store.RunStatusRunning {
		return r, false, nil
	}
	// If the manager already tracks this run, it's live in this
	// process — leave it alone, resume will reject with the active
	// status error.
	if s.manager.Active(runID) {
		return r, false, nil
	}
	lock, err := s.store.LockRun(context.Background(), runID)
	if err != nil {
		// Lock held by a real process — skip reconcile.
		return r, false, nil
	}
	// Re-read under the lock in case another writer raced us.
	r2, err := s.store.LoadRun(context.Background(), runID)
	if err != nil || r2.Status != store.RunStatusRunning {
		_ = lock.Unlock()
		if err != nil {
			return r, false, nil
		}
		return r2, false, nil
	}
	newStatus := store.RunStatusFailed
	if r2.Checkpoint != nil {
		newStatus = store.RunStatusFailedResumable
	} else {
		// No checkpoint means the run died before any node finished —
		// resume from entry is now possible thanks to the engine-side
		// permissive-restart path. Flag as resumable too so the studio
		// can offer the resume button.
		newStatus = store.RunStatusFailedResumable
	}
	const reason = "orphan reconciled on resume request: server had no live goroutine for run"
	if err := s.store.UpdateRunStatus(context.Background(), runID, newStatus, reason); err != nil {
		_ = lock.Unlock()
		return r2, false, fmt.Errorf("reconcile %s: %w", runID, err)
	}
	_ = lock.Unlock()
	if s.logger != nil {
		s.logger.Info("runview: reconciled orphan run %s on demand → %s", runID, newStatus)
	}
	r3, _ := s.store.LoadRun(context.Background(), runID)
	if r3 == nil {
		return r2, true, nil
	}
	return r3, true, nil
}
