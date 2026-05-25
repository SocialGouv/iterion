package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// cancelGracePeriod is how long we wait after firing the engine-level
// context cancel before force-removing the sandbox container as a
// belt-and-braces guarantee. Short enough that the operator sees the
// run end within a single attention span, long enough to let the
// engine's defer chain (sandbox.Cleanup) tear down cleanly on the
// happy path (in which case our docker rm just hits "No such
// container" and no-ops).
const cancelGracePeriod = 3 * time.Second

// forceRemoveSandboxContainer is the belt-and-braces cleanup for the
// /cancel API path. Without it, `claude --print` running inside a long-
// lived sandbox container often survives the engine context-cancel
// chain (docker exec dies on host, but dockerd doesn't always propagate
// SIGKILL into the container's PID namespace in time) — operators see
// the UI report "cancel requested" and the bot keeps streaming events
// for up to a minute. Finding
// `2026-05-25-dispatcher-cancel-doesnt-stop-claude-subprocess.md`
// captures the user-facing surface.
//
// Naming convention: the docker driver names every per-run container
// `iterion-<RunID>` (see `pkg/sandbox/docker/driver.go`); this helper
// targets that pattern via `docker rm --force`. Best-effort: a missing
// docker binary, a no-such-container error, or any other docker-side
// failure is logged at debug and never propagates — the engine's own
// cleanup may have already done the job, in which case this hammer is
// a no-op.
func forceRemoveSandboxContainer(logger *iterlog.Logger, runID string) {
	if runID == "" {
		return
	}
	containerName := "iterion-" + runID
	// Split stdout (printed container ID on a real removal) from stderr
	// (where docker writes "Error response from daemon: No such
	// container" on a missing target). Modern docker (29.x) exits 0 in
	// both cases, so the only reliable discriminator is which stream
	// got the output. Combined output would lump the two and break the
	// "happy path on missing container" detection.
	cmd := exec.Command("docker", "rm", "--force", containerName)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	stdoutTrim := strings.TrimSpace(stdout.String())
	stderrTrim := strings.TrimSpace(stderr.String())
	if strings.Contains(stderrTrim, "No such container") || strings.Contains(stderrTrim, "no such container") {
		// Happy path: engine's defer sandbox.Cleanup beat us to it.
		// Docker silently exits 0 in this case on 29.x, errors on
		// older releases — both end up here.
		return
	}
	if err != nil {
		if logger != nil {
			logger.Debug("dispatcher: force-remove %s after cancel: %v (stderr: %s)", containerName, err, stderrTrim)
		}
		return
	}
	if stdoutTrim == "" {
		// Edge: clean exit, no output, no "no such container" stderr.
		// Treat as no-op rather than claiming a removal we can't prove.
		return
	}
	if logger != nil {
		logger.Info("dispatcher: force-removed sandbox container %s after cancel", containerName)
	}
}

// cmd is the typed command interface processed by the actor goroutine.
// Each implementation is a small struct so command intent is visible
// in stack traces and log lines.
type cmd interface {
	apply(c *Dispatcher, ctx context.Context)
}

// cmdRefresh forces an immediate poll tick.
type cmdRefresh struct{}

func (cmdRefresh) apply(c *Dispatcher, ctx context.Context) { c.tick(ctx) }

// cmdSnapshot returns the current snapshot via reply channel.
type cmdSnapshot struct {
	reply chan Snapshot
}

func (m cmdSnapshot) apply(c *Dispatcher, _ context.Context) {
	m.reply <- c.buildSnapshot()
}

// cmdReload swaps in a new validated config.
type cmdReload struct {
	cfg *Config
}

func (m cmdReload) apply(c *Dispatcher, _ context.Context) {
	old := c.cfg.Load()
	c.cfg.Store(m.cfg)
	// c.hooks is no longer the source of truth — worker goroutines
	// read hooks via c.cfg.Load().Hooks each time so the atomic
	// pointer swap above is the single visibility boundary. See
	// F-CD-10: the previous parallel c.hooks write was racy against
	// worker reads.
	if old.PollingInterval() != m.cfg.PollingInterval() {
		c.logger.Info("dispatcher: polling interval %s → %s", old.PollingInterval(), m.cfg.PollingInterval())
	}
	c.fireSnapshot()
}

// cmdEvent updates the last-event watermark on a running issue.
type cmdEvent struct {
	issueID   string
	eventName string
}

func (m cmdEvent) apply(c *Dispatcher, _ context.Context) {
	r, ok := c.state.running[m.issueID]
	if !ok {
		return
	}
	r.LastEventAt = time.Now().UTC()
	r.LastEventName = m.eventName
}

// cmdRunFinished is posted by the dispatch goroutine when the run
// returns from Runner.Dispatch. err is non-nil on failure (including
// cancellation). The actor decides between continuation, retry, or
// release.
type cmdRunFinished struct {
	issueID string
	err     error
}

func (m cmdRunFinished) apply(c *Dispatcher, ctx context.Context) {
	c.finishRun(ctx, m.issueID, m.err)
	// The worker has now actually exited (this command is sent from
	// the dispatch goroutine's defer). Clear any tombstone refreshRunningStates
	// may have planted while the worker was draining so the next tick
	// can re-dispatch the issue when it reappears on the tracker side.
	delete(c.state.tombstones, m.issueID)
}

// finishRun is the actor-goroutine-side teardown for a running entry.
// Idempotent — a second call (e.g. the worker eventually returns and
// posts cmdRunFinished after refreshRunningStates already reaped the
// slot) finds c.state.running[issueID] empty and returns.
//
// Called both by cmdRunFinished.apply (the normal worker-return path)
// and by refreshRunningStates when an issue disappears from the
// tracker — without the second path, a worker that swallows ctx
// cancellation would leave the slot held indefinitely and the
// dispatcher would eventually starve itself out of concurrency budget.
func (c *Dispatcher) finishRun(ctx context.Context, issueID string, err error) {
	r, ok := c.state.running[issueID]
	if !ok {
		return
	}
	delete(c.state.running, issueID)
	if r.WorkflowState != "" {
		c.state.slotsByState[r.WorkflowState]--
		if c.state.slotsByState[r.WorkflowState] <= 0 {
			delete(c.state.slotsByState, r.WorkflowState)
		}
	}
	// Always release the tracker claim — if the issue is still active
	// on the tracker side, the next tick will re-pick it (unless we
	// schedule a retry below).
	//
	// Detach the release from the caller's ctx: refreshRunningStates
	// may invoke finishRun on the actor's ctx which is itself in the
	// shutdown-cancel state, and we don't want the tracker.Release
	// to short-circuit just because the dispatcher is winding down —
	// a stuck "claimed" label on GitHub blocks the next dispatcher
	// from re-picking the issue until the label is manually removed.
	relCtx, relCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if relErr := c.tracker.Release(relCtx, issueID, c.hostMarker); relErr != nil &&
		!errors.Is(relErr, tracker.ErrNotFound) &&
		!errors.Is(relErr, tracker.ErrClaimConflict) {
		c.logger.Warn("dispatcher: release %s: %v", r.Identifier, relErr)
	}

	currentTarget := c.cfg.Load().Agent.RunningState
	_ = ctx // caller ctx not used for the release; kept in signature for future audit hooks

	// Stamp the run + workdir back onto the tracker issue so the studio
	// can pivot from the kanban card to the run console / diff inspector.
	// Best-effort: only native trackers implement this, and a transient
	// disk failure shouldn't block the cleanup path.
	c.stampLastRun(issueID, r)

	switch {
	case err == nil:
		c.logger.Info("dispatcher: %s finished cleanly (run=%s)", r.Identifier, r.RunID)
		// Successful dispatches clear any prior retry bookkeeping and
		// honor the workspace-persist policy. We don't revert the
		// in-progress transition — the workflow may have moved the
		// state itself (e.g. doc-align → "review"). When the workflow
		// did NOT move the state (most often because it lacks
		// board.move capability — dispatcher_default is the
		// archetypal case), an explicit move to CompletedState here
		// prevents the next tick from re-picking the same issue:
		// RunningState is marked eligible:true on the board (needed
		// for crash-recovery), so without this transition a
		// no-board-move workflow would loop indefinitely and burn
		// model spend on every poll interval. Disabled when
		// CompletedState is empty (Validate maps "none" to "") or
		// when CompletedState == RunningState (the transition would
		// be a no-op anyway).
		if cur, ok := c.state.retries[issueID]; ok {
			if cur.Timer != nil {
				cur.Timer.Stop()
			}
			delete(c.state.retries, issueID)
		}
		c.maybeTransitionToCompleted(relCtx, issueID, r.Identifier, currentTarget)
		c.cleanupWorkspace(r)
	case errors.Is(err, context.Canceled):
		// Cancellation is a soft stop. Keep the workspace and any
		// pending retry entry so the next tick can re-pick the issue.
		// Revert the in-progress transition so the next dispatch sees
		// the issue back in its source state (typically "ready"). The
		// safety check inside revertTransition skips when the workflow
		// or operator already moved the state elsewhere.
		c.logger.Info("dispatcher: %s cancelled (run=%s)", r.Identifier, r.RunID)
		c.revertTransition(relCtx, issueID, r.Identifier, r.TransitionedFromState, currentTarget)
	default:
		// Non-cancellation failure → schedule a retry. Revert the
		// in-progress transition so the next retry tick sees the issue
		// eligible again from its source state. Without the revert,
		// the issue would sit in `in_progress` (no longer in the
		// eligible "ready" set) until the operator dragged it back.
		c.logger.Warn("dispatcher: %s failed (run=%s): %v", r.Identifier, r.RunID, err)
		c.revertTransition(relCtx, issueID, r.Identifier, r.TransitionedFromState, currentTarget)
		c.scheduleRetry(issueID, r, err)
	}
	relCancel()
	c.fireSnapshot()
}

// stampLastRun records the (run_id, workdir) pair on the tracker
// issue so the studio's IssueModal can link back to the most recent
// run that processed it. Only native trackers implement the
// SetLastRun shape — external trackers (github, forgejo) silently
// skip via the failed type-assertion. Best-effort: a write failure
// is logged at warn and does not derail cleanup.
//
// The workdir is read from <storeDir>/runs/<runID>/run.json when
// available (canonical: this reflects worktree:auto's swap to the
// per-run worktree path). On read failure we fall back to the
// dispatcher's pre-run WorkspacePath so the operator still gets a
// useful path to inspect.
func (c *Dispatcher) stampLastRun(issueID string, r *runningEntry) {
	setter, ok := c.tracker.(interface {
		SetLastRun(id, runID, workdir string) error
	})
	if !ok {
		return
	}
	workdir := c.resolveRunWorkdir(r.RunID)
	if workdir == "" {
		workdir = r.WorkspacePath
	}
	if err := setter.SetLastRun(issueID, r.RunID, workdir); err != nil {
		c.logger.Warn("dispatcher: stamp last-run on %s: %v", r.Identifier, err)
	}
}

// maybeTransitionToCompleted moves a cleanly-finished issue from
// RunningState into CompletedState when the workflow itself didn't
// move it elsewhere. No-op when:
//   - CompletedState is empty (operator opted out via `none`).
//   - CompletedState equals RunningState (the transition would be
//     a no-op anyway, and trackers may reject same-state moves).
//   - The current tracker state differs from RunningState — that's
//     the "workflow already moved it" case (doc-align → review,
//     a board-aware bot picking "done", etc.); leave it alone.
//   - Tracker rejects the transition (state not defined on the
//     board, blocking guard, etc.) — log + leave in RunningState.
//     Operators with custom boards aren't forced to opt out.
//
// The motivation lives in the cfg.Agent.CompletedState comment in
// config.go; this helper is just the application path. Detached
// ctx is the caller's relCtx so a winding-down dispatcher still
// completes the move (parallels the Release path).
func (c *Dispatcher) maybeTransitionToCompleted(ctx context.Context, issueID, identifier, runningTarget string) {
	cfg := c.cfg.Load()
	completed := cfg.Agent.CompletedState
	if completed == "" || completed == runningTarget {
		return
	}
	states, err := c.tracker.RefreshStates(ctx, []string{issueID})
	if err != nil {
		c.logger.Debug("dispatcher: completed-state probe %s: %v", identifier, err)
		return
	}
	cur, ok := states[issueID]
	if !ok {
		// Issue disappeared from the tracker — refreshRunningStates'
		// tombstone path already handled its cleanup; nothing to do.
		return
	}
	if cur != runningTarget {
		// Workflow (or operator) already moved the state. Honor it.
		return
	}
	if err := c.tracker.UpdateState(ctx, issueID, completed); err != nil {
		if errors.Is(err, tracker.ErrTransitionRejected) || errors.Is(err, tracker.ErrNotSupported) {
			c.logger.Info("dispatcher: %s stayed in %s (tracker rejected move to %q): %v", identifier, runningTarget, completed, err)
			return
		}
		c.logger.Warn("dispatcher: completed-state move %s → %s: %v", identifier, completed, err)
		return
	}
	c.logger.Info("dispatcher: %s moved %s → %s (workflow didn't change state, default auto-transition)", identifier, runningTarget, completed)
}

// resolveRunWorkdir reads the run's persisted WorkDir from
// <storeDir>/runs/<runID>/run.json. Returns "" when storeDir is
// unset, the file is missing, or the JSON can't be decoded — every
// call site treats "" as "fall back to the dispatcher workspace".
func (c *Dispatcher) resolveRunWorkdir(runID string) string {
	if c.storeDir == "" || runID == "" {
		return ""
	}
	path := filepath.Join(c.storeDir, "runs", runID, "run.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var probe struct {
		WorkDir string `json:"work_dir"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	return probe.WorkDir
}

// cleanupWorkspace removes the per-issue workspace directory when the
// active persist policy calls for it. Best-effort — failures are logged.
func (c *Dispatcher) cleanupWorkspace(r *runningEntry) {
	if !c.cfg.Load().Workspace.Persist.shouldCleanupOnSuccess() {
		return
	}
	if err := c.workspaces.Remove(r.IssueID); err != nil {
		c.logger.Warn("dispatcher: cleanup workspace %s: %v", r.Identifier, err)
	}
}

// cmdRetryDue fires when a retry timer expires. We simply drop the
// retry entry so the issue becomes eligible again; the next polling
// tick redispatches. We intentionally do NOT trigger an immediate
// tick — if N retries fire simultaneously, the regular ticker handles
// them in one pass instead of running N back-to-back ListCandidates
// calls on the actor goroutine.
type cmdRetryDue struct {
	issueID string
}

func (m cmdRetryDue) apply(c *Dispatcher, _ context.Context) {
	if cur, ok := c.state.retries[m.issueID]; ok {
		if cur.Timer != nil {
			cur.Timer.Stop()
		}
		// Keep the entry around so the next tick's dispatch can read
		// the Attempt count. The Fired flag flips isClaimed off so the
		// issue becomes eligible again; dispatch then consumes the
		// entry as it constructs the runningEntry.
		cur.Fired = true
	}
}

// cmdCancel cancels an in-flight run from outside (HTTP handler).
type cmdCancel struct {
	issueID string
}

func (m cmdCancel) apply(c *Dispatcher, _ context.Context) {
	r, ok := c.state.running[m.issueID]
	if !ok {
		return
	}
	if r.Cancel != nil {
		r.Cancel()
	}
	c.logger.Info("dispatcher: %s cancel requested", r.Identifier)
	// Belt-and-braces: after a short grace period for the engine's
	// defer chain to clean up, force-remove any lingering sandbox
	// container. Without this, `claude --print` inside the container
	// often outlives the host-side docker exec SIGKILL and keeps
	// streaming events for tens of seconds — operators perceive cancel
	// as broken.
	runID := r.RunID
	logger := c.logger
	go func() {
		time.Sleep(cancelGracePeriod)
		forceRemoveSandboxContainer(logger, runID)
	}()
}

// cmdCancelByRunID cancels an in-flight run identified by its RunID
// (rather than issueID). Used by the run console's HTTP cancel handler,
// which doesn't know which issue produced the run. Returns true on the
// reply channel iff a matching entry was found and signalled.
type cmdCancelByRunID struct {
	runID string
	reply chan bool
}

func (m cmdCancelByRunID) apply(c *Dispatcher, _ context.Context) {
	for _, r := range c.state.running {
		if r.RunID != m.runID {
			continue
		}
		if r.Cancel != nil {
			r.Cancel()
		}
		c.logger.Info("dispatcher: %s cancel requested (run %s)", r.Identifier, m.runID)
		runID := r.RunID
		logger := c.logger
		go func() {
			time.Sleep(cancelGracePeriod)
			forceRemoveSandboxContainer(logger, runID)
		}()
		m.reply <- true
		return
	}
	m.reply <- false
}
