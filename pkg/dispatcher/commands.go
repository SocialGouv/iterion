package dispatcher

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

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
	relCancel()
	_ = ctx // caller ctx not used for the release; kept in signature for future audit hooks

	switch {
	case err == nil:
		c.logger.Info("dispatcher: %s finished cleanly (run=%s)", r.Identifier, r.RunID)
		// Successful dispatches clear any prior retry bookkeeping and
		// honor the workspace-persist policy.
		if cur, ok := c.state.retries[issueID]; ok {
			if cur.Timer != nil {
				cur.Timer.Stop()
			}
			delete(c.state.retries, issueID)
		}
		c.cleanupWorkspace(r)
	case errors.Is(err, context.Canceled):
		// Cancellation is a soft stop. Keep the workspace and any
		// pending retry entry so the next tick can re-pick the issue.
		c.logger.Info("dispatcher: %s cancelled (run=%s)", r.Identifier, r.RunID)
	default:
		c.logger.Warn("dispatcher: %s failed (run=%s): %v", r.Identifier, r.RunID, err)
		c.scheduleRetry(issueID, r, err)
	}
	c.fireSnapshot()
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
		m.reply <- true
		return
	}
	m.reply <- false
}
