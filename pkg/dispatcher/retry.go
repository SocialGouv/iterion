package dispatcher

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// sandboxBackoffSchedule overrides the default exponential backoff
// when the failure is a sandbox-setup error (devcontainer postCreate
// killed, docker daemon refused, image pull failed). These don't
// recover within milliseconds — the host is the bottleneck. Pause
// longer between attempts so the operator's docker daemon and OS
// have room to breathe, and so a runaway OOM cycle doesn't pin the
// dispatcher in a "spawn-die-spawn-die" loop that exhausts further
// resources.
var sandboxBackoffSchedule = []time.Duration{
	60 * time.Second,
	180 * time.Second,
	300 * time.Second,
}

// sandboxParkDelay is the delay queued AFTER the schedule is
// exhausted. We don't strictly stop retrying (the operator may fix
// the host without touching the dispatcher), but we wait long enough
// that the model spend / docker churn settles to zero. A retry entry
// with a 1h DueAt is also visible on the studio's dispatcher view so
// the operator can manually clear it after fixing the underlying
// issue.
const sandboxParkDelay = 1 * time.Hour

// scheduleRetry queues a retry for the given issue, using exponential
// backoff capped by cfg.MaxRetryBackoff. Must be called from the actor.
//
// When the prior run terminated in a resumable status (failed_resumable,
// cancelled, paused_operator), prev.RunID is captured on the retry
// entry so the next dispatch resumes the same run via
// runtime.Engine.Resume instead of minting a fresh one. A live last_run
// is never discarded to mint a sibling planner from entry — see
// lastRunForbidsFresh. The engine's resume machinery picks up at the
// failing node, reuses the worktree the prior run created, and avoids
// re-executing upstream nodes (which can be expensive — a feature_dev
// plan node can spend $5 and 10 minutes on workspace exploration
// before producing its first artifact).
func (c *Dispatcher) scheduleRetry(issueID string, prev *runningEntry, runErr error) {
	cfg := c.cfg.Load()
	// prevAttempt is the attempt index of the run that just failed. It
	// lives on the runningEntry — dispatch() carries it forward from the
	// consumed retry entry — so reading it here is what makes the counter
	// ACCUMULATE across retries. Reading c.state.retries[issueID] alone (the
	// prior behaviour) always missed: dispatch() deletes that entry when it
	// picks the run up, so prevAttempt reset to 0 every cycle and the
	// attempt count was frozen at 1 — the dashboard never advanced past
	// "attempt 1" and any attempt ceiling would never trigger.
	prevAttempt := prev.Attempt
	if cur, ok := c.state.retries[issueID]; ok {
		if cur.Attempt > prevAttempt {
			prevAttempt = cur.Attempt
		}
		if cur.Timer != nil {
			cur.Timer.Stop()
		}
	}
	attempt := prevAttempt + 1
	delay := computeBackoff(attempt, cfg.MaxRetryBackoff())
	sandboxFail := isSandboxSetupError(runErr)
	parked := false
	if sandboxFail {
		var sbParked bool
		delay, sbParked = sandboxBackoff(attempt)
		parked = sbParked
	}
	due := time.Now().Add(delay)

	timer := time.AfterFunc(delay, func() {
		select {
		case c.cmds <- cmdRetryDue{issueID: issueID}:
		case <-c.stop:
		}
	})
	errStr := ""
	if runErr != nil {
		errStr = runErr.Error()
	}
	prevRunID := c.resumableRunID(prev.RunID)
	// Source-changed is parked in finishRun (keep last_run, no sibling).
	// If it still reaches here, keep the resume pointer: minting a
	// planner from entry is worse than retrying a doomed resume.
	// lastRunForbidsFresh still refuses GenerateRunID.
	c.state.retries[issueID] = &retryEntry{
		IssueID:    issueID,
		Identifier: prev.Identifier,
		Attempt:    attempt,
		DueAt:      due,
		LastError:  errStr,
		Timer:      timer,
		PrevRunID:  prevRunID,
	}
	switch {
	case parked:
		c.logger.Warn("dispatcher: %s parked after %d sandbox-setup failures — next retry in %s. Investigate host (docker daemon, OOM, disk) before then or clear the retry from the studio.", prev.Identifier, attempt-1, delay)
	case sandboxFail:
		c.logger.Warn("dispatcher: %s sandbox setup failed (attempt=%d/%d) — backing off %s before retry", prev.Identifier, attempt, len(sandboxBackoffSchedule), delay)
	default:
		c.logger.Info("dispatcher: %s retry queued (attempt=%d, in=%s, resume=%s)", prev.Identifier, attempt, delay, prev.RunID)
	}
}

// isResumeSourceChanged reports whether runErr is the runtime's refusal
// to resume because the bot's workflow source changed since the prior
// run started (pkg/runtime/resume.go: "workflow source has changed ...
// re-run from scratch or use --force"). finishRun parks the ticket
// instead of minting a sibling: the operator resumes THIS run with
// --force. Cancelling is not an escape: cancelled last_runs are resumed
// from their checkpoint and still forbid a fresh sibling. The runtime
// exposes a typed sentinel in-process and retains a compatibility
// fallback for detached/mixed-version boundaries that flatten errors
// to text.
func isResumeSourceChanged(err error) bool {
	return runtime.IsWorkflowSourceChanged(err)
}

// lastRunForbidsFresh reports statuses that still own the ticket: the
// dispatcher must re-park, resume, or hold — never mint a sibling
// planner from the workflow entry. finished is deliberately NOT in the
// set: the work completed, and dragging the card back to an eligible
// column is the operator's deliberate re-queue gesture, and forbidding
// finished would make a fresh run for that card unobtainable for as
// long as the pointer stands. The other legitimate fresh starts are no
// last_run, or a hard-failed / vanished last_run plus an explicitly
// eligible ticket.
//
// ONE surface clears the pointer deliberately — `iterion issue update
// --clear-last-run`, the operator's way out of a run that resuming cannot
// fix. It refuses while that run is still alive (running / queued /
// paused_*), precisely so it cannot hand this guard an empty pointer for a
// ticket somebody is still working on.
func lastRunForbidsFresh(status store.RunStatus) bool {
	switch status {
	case store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
		store.RunStatusRunning,
		store.RunStatusQueued,
		store.RunStatusFailedResumable,
		store.RunStatusCancelled:
		return true
	default:
		return false
	}
}

// orphanRunGraceWindow shields a just-created running run from the dead-owner
// probe: its launcher may sit between the run-record write and the lock
// acquisition, so a young run is never judged. Mirrors
// runview's orphanGraceWindow (pkg/runview/service_lifecycle.go).
const orphanRunGraceWindow = 2 * time.Minute

// resumableRunID returns the runID iff the corresponding run record
// can be resumed by an already-authorized dispatcher retry — i.e. its
// on-disk status is failed_resumable, cancelled, or paused_operator.
// On restart, reparkClaimedIfLastRunWaiting intercepts dispatcher-owned
// paused_operator before this helper is reached; the status remains here for
// an in-memory retry decision in the same process. paused_waiting_human is
// deliberately excluded (no answers → immediate re-pause); that status is
// re-parked, not resumed. Returns "" if the run is missing,
// hard-failed, finished, or any error reading the store. An empty
// result is NOT a licence to mint a sibling: resolveRunID still
// consults lastRunForbidsFresh before GenerateRunID.
//
// A running status first goes through the dead-owner probe
// (promoteIfOrphaned): the only orphan reaper lives in
// runview.Service.reconcileOrphans, which `iterion dispatch
// --no-server` never runs — without a dispatcher-side probe a run left
// "running" on disk by a SIGKILL/host crash would hold its ticket
// forever. Local disk I/O only; context.Background is fine on the
// actor per the ADR-028 Step 3 boundary (same as runStatusOnDisk).
// Best-effort: store IO errors are debug-logged, never fatal.
func (c *Dispatcher) resumableRunID(runID string) string {
	if runID == "" || c.storeDir == "" {
		return ""
	}
	s, err := store.New(c.storeDir, store.WithLogger(c.logger))
	if err != nil {
		c.logger.Debug("dispatcher: open store for resume check: %v", err)
		return ""
	}
	ctx := context.Background()
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		c.logger.Debug("dispatcher: cannot read run %s for resume check: %v", runID, err)
		return ""
	}
	status := r.Status
	if status == store.RunStatusRunning {
		status = c.promoteIfOrphaned(ctx, s, r)
	}
	switch status {
	case store.RunStatusFailedResumable,
		store.RunStatusCancelled,
		store.RunStatusPausedOperator:
		return runID
	}
	return ""
}

// promoteIfOrphaned re-examines a running run whose owner may be
// dead, mirroring runview.Service.reconcileOrphans: a just-created run
// is shielded by the grace window, then a non-blocking LockRun is the
// liveness probe — grabbing it proves no live process owns the run
// (both the dispatcher's engine_runner and runview launches lock their
// run for its whole lifetime, and flock is auto-released on crash). A
// run nobody holds is promoted to failed_resumable (checkpoint present
// → the caller resumes it) or failed (no recovery point → a fresh run
// becomes legitimate), and the new status is returned. A held lock
// (live owner), a store without cross-process lock authority, or any
// error leaves the status untouched — the ticket is held, never
// clobbered.
func (c *Dispatcher) promoteIfOrphaned(ctx context.Context, s *store.FilesystemRunStore, r *store.Run) store.RunStatus {
	status := r.Status
	if !s.Capabilities().CrossProcessLock {
		return status
	}
	if time.Since(r.CreatedAt) < orphanRunGraceWindow {
		return status
	}
	lock, err := s.LockRun(ctx, r.ID)
	if err != nil {
		return status // a live owner holds the lock
	}
	defer func() { _ = lock.Unlock() }()
	// Re-load under the lock — the owner may have released between the
	// first read and now after persisting a terminal status.
	cur, err := s.LoadRun(ctx, r.ID)
	if err != nil {
		return status
	}
	// Queued runs intentionally have no lock owner while they wait for a
	// pipelines concurrency slot. A free lock is therefore evidence of an
	// orphan only for running, matching runview.Service.reconcileOrphans.
	if cur.Status != store.RunStatusRunning {
		return cur.Status
	}
	newStatus := store.RunStatusFailed
	if cur.Checkpoint != nil {
		newStatus = store.RunStatusFailedResumable
	}
	if err := s.UpdateRunStatus(ctx, cur.ID, newStatus, "process orphaned: dispatcher found run '"+string(cur.Status)+"' with no live owner"); err != nil {
		c.logger.Debug("dispatcher: orphan promotion %s: %v", cur.ID, err)
		return status
	}
	c.logger.Info("dispatcher: last run %s was %s with no live owner — promoted to %s", cur.ID, cur.Status, newStatus)
	return newStatus
}

// isSandboxSetupError reports whether the run failed before the
// runtime engine got to execute any node — devcontainer postCreate
// exited non-zero, docker daemon refused, image pull timed out. These
// are NOT transients the per-node recovery dispatch can mask; they
// fail deterministically until the host is fixed. The dispatcher
// applies sandboxBackoffSchedule instead of the default exponential
// so consecutive failures don't pile docker churn on a stressed host.
//
// Match strings are intentionally broad and lowercase — claw's claude
// CLI, claude_code, the runtime's sandbox driver, and a few buildkit
// edge cases all wrap their errors with slightly different prefixes,
// and matching too tightly here means a stress-induced postCreate
// failure slips into the default 10s exponential and re-spawns the
// container before the host has recovered.
func isSandboxSetupError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "sandbox start:") {
		return true
	}
	if strings.Contains(msg, "postcreate") {
		return true
	}
	if strings.Contains(msg, "post_create") {
		return true
	}
	if strings.Contains(msg, "image pull") {
		return true
	}
	if strings.Contains(msg, "container start") {
		return true
	}
	// A broken/partial CLI install inside the sandbox surfaces as an
	// "exec format error" when the runtime first invokes it (observed:
	// npm install -g claude-code exits 0 leaving a claude.exe symlink
	// whose target wasn't fully written → EFORMAT on the first
	// claude_code node — native:c6d93a2a). The hardened post_create now
	// fails the boot cleanly, but if a broken binary still slips through
	// to node execution, treat it as a sandbox-setup error so the retry
	// uses the backoff + a fresh container (where the reinstall takes)
	// rather than the default exponential against the same broken image.
	if strings.Contains(msg, "exec format error") {
		return true
	}
	return false
}

// sandboxBackoff returns the delay for the given retry attempt under
// the sandbox-setup-error schedule + a parked flag once the schedule
// is exhausted. attempt is 1-indexed (first retry = attempt 1).
func sandboxBackoff(attempt int) (delay time.Duration, parked bool) {
	if attempt < 1 {
		return time.Second, false
	}
	if attempt <= len(sandboxBackoffSchedule) {
		return sandboxBackoffSchedule[attempt-1], false
	}
	return sandboxParkDelay, true
}

// computeBackoff returns min(10s * 2^(attempt-1), cap), with attempt=0
// treated as a continuation (fixed 1s).
func computeBackoff(attempt int, cap time.Duration) time.Duration {
	if attempt <= 0 {
		return time.Second
	}
	const base = 10 * time.Second
	// Cap the exponent to avoid int overflow on absurd attempt counts.
	if attempt > 10 {
		attempt = 10
	}
	mult := math.Pow(2, float64(attempt-1))
	d := time.Duration(float64(base) * mult)
	if cap > 0 && d > cap {
		return cap
	}
	return d
}
