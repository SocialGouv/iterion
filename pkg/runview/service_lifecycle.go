package runview

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	dockersandbox "github.com/SocialGouv/iterion/pkg/sandbox/docker"
	k8ssandbox "github.com/SocialGouv/iterion/pkg/sandbox/kubernetes"
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
	// Same liveness-authority guard as reconcileOrphans (b7b63f723): the
	// reap probes LockRun to decide "owner gone", but a store with no real
	// cross-process lock (the cloud server's noop lock) always "succeeds"
	// the probe — so without this a server sharing a store with docker
	// runners on one host could force-remove live sandbox containers.
	// Today this is masked (cloud servers have no docker daemon, so Detect
	// below no-ops), but gate it explicitly so the two reapers can't drift.
	if !s.store.Capabilities().CrossProcessLock {
		return
	}
	rt, err := dockersandbox.Detect()
	if err != nil {
		return
	}
	// Boot-time admin scan: peek at runs across tenants to decide
	// whether their docker leftovers should be reaped.
	ctx := store.WithoutTenantFilter(context.Background())
	reaped, err := dockersandbox.ReapOrphanContainers(ctx, rt, func(runID string) bool {
		return s.sandboxContainerReapable(ctx, runID)
	})
	if err != nil {
		s.logger.Warn("runview: reap orphan containers: %v", err)
	}
	if len(reaped) > 0 {
		s.logger.Info("runview: reaped %d orphan sandbox container(s)", len(reaped))
	}
}

// reconcileSandboxK8sResources force-deletes iterion-managed kubernetes
// sandbox resources (pod + CA/file-secrets Secret + NetworkPolicy) whose
// owning run has reached a terminal status or vanished from the store.
// The kubernetes counterpart to reconcileSandboxContainers: a runner pod
// SIGKILLed / OOM-killed / node-evicted mid-run never runs Run.Cleanup,
// so its sandbox footprint — including the Secret holding plaintext BYOK/
// forge credentials — leaks with no TTL. The self-terminating manifest
// (ownerReference + activeDeadlineSeconds) covers the runner-pod-deleted
// and idle-forever cases; this sweep closes the rest.
//
// Gated on the SAME cross-process-lock authority as the docker reaper: a
// store with a noop lock (the lock-less cloud server) always "succeeds"
// the liveness probe, so without this gate a server sharing a namespace
// with live runner pods could reap an in-flight run's sandbox. Safe when
// not in-cluster: kubernetes.Detect returns an error we swallow.
func (s *Service) reconcileSandboxK8sResources() {
	if !s.store.Capabilities().CrossProcessLock {
		return
	}
	_, namespace, err := k8ssandbox.Detect()
	if err != nil {
		return // not an in-cluster runner — nothing to reconcile
	}
	// Boot-time admin scan: peek at runs across tenants to decide whether
	// their kubernetes leftovers should be reaped. Reuses the exact same
	// reapability predicate as the docker reaper (liveness-first).
	ctx := store.WithoutTenantFilter(context.Background())
	reaped, err := k8ssandbox.ReapOrphanResources(ctx, namespace, func(runID string) bool {
		return s.sandboxContainerReapable(ctx, runID)
	})
	if err != nil {
		s.logger.Warn("runview: reap orphan k8s sandbox resources: %v", err)
	}
	if len(reaped) > 0 {
		s.logger.Info("runview: reaped %d orphan k8s sandbox resource(s)", len(reaped))
	}
}

// sandboxContainerReapable is the isTerminal predicate for
// reconcileSandboxContainers — reports whether a managed sandbox
// container's owning run is gone, so its container may be force-removed.
// Extracted from the reaper closure for unit testing.
//
// Liveness FIRST: a non-blocking run lock that FAILS to acquire proves
// the owning process is still alive, so we NEVER reap a live run's
// container — its in-flight docker exec(s) (claude_code / claw delegate
// calls) would otherwise die with a baffling "No such container" mid-run.
// This guards every concurrent owner: a CLI `iterion run` sharing this
// store dir holds the run lock for its whole execution
// (pkg/cli/run.go: LockRun + defer Unlock), so a daemon restart's
// boot-time reap (e.g. studio:dev bouncing under watchexec while a
// dogfood run is live in the same store) cannot kill it. This is safer
// than — and independent of — the status check, which can't see a status
// that is mid-write or briefly unreadable.
//
// CROSS-STORE: LockRun/LoadRun key on this store's root and ignore the
// tenant ctx, so a run living under a DIFFERENT project/store root (a CLI
// run dogfooding in another project) is invisible to the lock probe. For
// that case the LoadRun-failure branch below is the backstop: a record
// absent from this store is treated as "not ours, don't touch", never as a
// reapable orphan — so a cross-project live run is never reaped either.
func (s *Service) sandboxContainerReapable(ctx context.Context, runID string) bool {
	if runID == "" {
		return true // managed container with no run owner → orphan
	}
	if lock, lockErr := s.store.LockRun(ctx, runID); lockErr != nil {
		return false // lock held → process alive → keep the container
	} else {
		_ = lock.Unlock() // lock free → owner gone; fall through to status
	}
	r, loadErr := s.store.LoadRun(ctx, runID)
	if loadErr != nil {
		// The run record is absent from THIS store. LoadRun and LockRun key
		// on this store's root and ignore the tenant ctx (pkg/store), so a
		// container whose run lives under a different project/store root lands
		// here — most commonly a concurrent `iterion run` dogfooding in
		// another project while the studio bounces under watchexec. This
		// service is not that run's authority and cannot see its lock, so
		// reaping would kill a live cross-project run mid-flight (observed:
		// scanner / voter nodes dying with "No such container" + exit 137).
		// Leave it: its own owner reaps it on exit, and a genuinely-dead
		// container is cleanable via `docker container prune`. Favour leaking
		// a container over killing a live run.
		return false
	}
	switch r.Status {
	case store.RunStatusRunning, store.RunStatusPausedWaitingHuman:
		return false
	default:
		return true
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
	// The reap uses LockRun as the liveness probe: grabbing the lock
	// proves no other process holds the run. That is only meaningful
	// when the store has a REAL cross-process lock (filesystem flock,
	// or the runner's NATS-KV lease). The cloud SERVER store has no
	// lock provider, so LockRun returns a noop that always "succeeds"
	// — which would make every runner-owned run in `running` look
	// orphaned and get reaped mid-flight (observed: a 60s tick failing
	// live cloud runs). When the store can't prove liveness, skip the
	// reap entirely: a genuinely dead runner is recovered by the NATS
	// lease expiring + JetStream redelivery, not by the server.
	canReap := s.store.Capabilities().CrossProcessLock
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
		//
		// Only when this service has liveness authority: the cloud server
		// (noop lock) owns no worktree — the finalize runs on the runner
		// pod. Calling it here reads a WorkDir/RepoRoot absent on the
		// server pod, doing pointless git work + logging a spurious
		// "cannot read worktree HEAD" every tick per finished run.
		if canReap {
			if recErr := runtime.RecoverFinalize(ctx, s.store, r, s.logger); recErr != nil {
				s.logger.Warn("runview: recover finalize %s: %v", id, recErr)
			}
		}
		if r.Status != store.RunStatusRunning {
			continue
		}
		// No liveness authority (cloud server, noop lock): never reap a
		// runner-owned run — RecoverFinalize above still ran for any
		// finished worktree run, which is all the server legitimately
		// needs to do here.
		if !canReap {
			continue
		}
		// In-process active run: this service owns it — skip before any
		// lock/PID probing. Matters for the periodic re-scan (the boot
		// scan predates any active run): without this guard every tick
		// would re-probe each managed run's flock and re-attempt
		// RegisterDetached on already-reattached detached runners.
		if s.manager.Active(id) {
			continue
		}
		// .pid present + PID alive → runner outlived the previous
		// server lifetime; re-attach. Stale .pid → remove and fall
		// through to the flock probe. Missing .pid → in-process or
		// older run; flock probe applies.
		if s.tryReattachByPID(id) {
			continue
		}
		// Grace window: a just-created run may sit between its run-record
		// write and its owner's lock acquisition, and the boot scan on a
		// large store can take long enough to REACH a run that was
		// launched after the scan began (observed: a dispatcher-owned run
		// flipped failed 16s after dispatch). Never judge a run this
		// young — the next tick re-probes it with the lock in place.
		// (Placed after the PID reattach so a live detached runner still
		// reattaches immediately regardless of age.)
		if time.Since(r.CreatedAt) < orphanGraceWindow {
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

// orphanGraceWindow shields freshly-created runs from the orphan scan:
// within it, "no lock held" is not yet proof of a dead owner.
const orphanGraceWindow = 2 * time.Minute

// defaultOrphanReconcileInterval is how often the periodic reconcile
// re-runs the orphan scan after boot. Overridable via
// ITERION_ORPHAN_RECONCILE_INTERVAL (a Go duration; "0" disables the
// ticker, keeping only the boot-time scan).
const defaultOrphanReconcileInterval = 60 * time.Second

func orphanReconcileInterval() time.Duration {
	v := os.Getenv("ITERION_ORPHAN_RECONCILE_INTERVAL")
	if v == "" {
		return defaultOrphanReconcileInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultOrphanReconcileInterval
	}
	return d // <= 0 disables
}

// startPeriodicReconcile re-runs reconcileOrphans on a ticker so a run
// whose owning process crashes while the service is up (a CLI `iterion
// run` sharing the store, a killed detached runner) flips to
// failed/failed_resumable within a minute instead of staying `running`
// until the next server restart. The scan is idempotent and
// liveness-gated (flock probe + manager.Active guard), so re-running it
// against live runs is a no-op. reconcileSandboxContainers is
// deliberately NOT on the tick — it would round-trip docker every
// interval; the boot scan plus owner-exit reaping cover containers.
//
// reconcileSandboxK8sResources IS on the tick, unlike its docker peer: a
// runner pod OOM-killed / node-evicted while THIS server stays up leaks a
// sandbox pod + its plaintext-credential Secret that boot-only reaping
// wouldn't catch until the next restart. It stays cheap and safe — the
// same lock-authority gate (off on the lock-less cloud server) and
// liveness-first predicate as the boot scan, no-op when not in-cluster.
func (s *Service) startPeriodicReconcile() {
	interval := orphanReconcileInterval()
	if interval <= 0 {
		return
	}
	s.reconcileStop = make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.reconcileStop:
				return
			case <-t.C:
				if s.draining.Load() {
					return
				}
				// Contain a reconcile panic: one bad run.json / driver
				// edge case must not silently kill orphan detection for
				// the rest of the process lifetime (the sweep IS the
				// safety net — a dead net is worse than a noisy one).
				func() {
					defer func() {
						if r := recover(); r != nil && s.logger != nil {
							s.logger.Error("runview: PANIC in periodic reconcile: %v\n%s", r, debug.Stack())
						}
					}()
					s.reconcileOrphans()
					s.reconcileSandboxK8sResources()
				}()
			}
		}
	}()
}

// stopPeriodicReconcile ends the reconcile goroutine. Idempotent —
// Stop and Drain may both run in one teardown.
func (s *Service) stopPeriodicReconcile() {
	s.reconcileStopOnce.Do(func() {
		if s.reconcileStop != nil {
			close(s.reconcileStop)
		}
	})
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
	s.stopPeriodicReconcile()
	s.stopPipelineScheduler()
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
	s.stopPeriodicReconcile()
	// Queued pipelines stay persisted as queued docs and are recovered on
	// the next boot, so stopping the scheduler here never strands them.
	s.stopPipelineScheduler()

	// Stop the alert manager's stall-poll goroutine. It was started with
	// context.Background() (so it outlives per-run contexts), so Drain is
	// the only place that reaps it — without this it leaks across project
	// hot-swaps that construct a fresh Service.
	if s.alertManager != nil {
		s.alertManager.Stop()
	}

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
		Data:  map[string]any{"reason": reason},
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
	// Both paths are resumable: with a checkpoint we resume from the failed
	// node; without one, the engine's permissive-restart path resumes from
	// entry — either way the studio can offer the resume button.
	newStatus := store.RunStatusFailedResumable
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
