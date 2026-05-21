package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// tick is the dispatcher's heartbeat. Runs entirely on the actor
// goroutine: reconciles stalled runs, refreshes tracker states for
// running issues, then dispatches new candidates until slots fill up.
//
// When the dispatcher is paused, the dispatch step is skipped — runs
// already in flight continue, stall detection still fires, retries
// still queue. Paused means "no new work", not "stop everything".
func (c *Dispatcher) tick(ctx context.Context) {
	cfg := c.cfg.Load()

	c.reconcileStalled(ctx, cfg)
	c.refreshRunningStates(ctx)

	if c.paused.Load() {
		c.fireSnapshot()
		return
	}

	// Skip the tracker call when we're already at the global cap —
	// any candidate we'd see is unactionable, and ListCandidates is
	// the slowest synchronous step inside the actor goroutine
	// (external HTTP for github/forgejo; in-memory but still locked
	// for native). Keeping the actor responsive for cmdRunFinished /
	// cmdEvent matters more than discovering candidates we can't
	// dispatch. Once an in-flight run finishes the next tick will
	// pick up the work.
	if len(c.state.running) >= cfg.Agent.MaxConcurrent {
		c.fireSnapshot()
		return
	}

	candidates, err := c.tracker.ListCandidates(ctx)
	if err != nil {
		c.logger.Warn("dispatcher: tracker ListCandidates: %v", err)
		c.state.lastTrackerErr = err.Error()
		c.state.lastTrackerErrAt = time.Now().UTC()
		c.fireSnapshot()
		return
	}
	// Clear the sticky tracker error once a poll succeeds so the
	// dashboard banner drops as soon as the operator fixes the token.
	c.state.lastTrackerErr = ""
	c.state.lastTrackerErrAt = time.Time{}
	sortCandidates(candidates)

	for _, iss := range candidates {
		// Global cap full → no further candidate can run; stop scanning.
		if len(c.state.running) >= cfg.Agent.MaxConcurrent {
			break
		}
		// Per-state cap full → skip this candidate but keep scanning;
		// other candidates may be in states that still have room.
		if !c.hasSlot(iss.WorkflowState, cfg) {
			continue
		}
		if c.state.isClaimed(iss.ID) {
			continue
		}
		c.dispatch(ctx, iss)
	}
	c.fireSnapshot()
}

func sortCandidates(in []tracker.Issue) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Priority != in[j].Priority {
			return in[i].Priority > in[j].Priority
		}
		if !in[i].CreatedAt.Equal(in[j].CreatedAt) {
			return in[i].CreatedAt.Before(in[j].CreatedAt)
		}
		return in[i].Identifier < in[j].Identifier
	})
}

// defaultStallReapGrace is how long reconcileStalled waits after
// issuing ctx cancellation before force-reaping the slot. A
// well-behaved backend exits within seconds of Cancel(); this grace
// covers the SIGTERM → SIGKILL ladder in the claudesdk close() path
// plus a small buffer for finalization. After the grace expires we
// plant a tombstone + finishRun (mirroring the refreshRunningStates
// path for tracker-disappeared issues) so a backend that swallows
// ctx can't pin a slot forever and starve max_concurrent.
//
// Override via ITERION_DISPATCHER_STALL_REAP_GRACE (Go duration).
const defaultStallReapGrace = 60 * time.Second

func resolveStallReapGrace() time.Duration {
	if v := os.Getenv("ITERION_DISPATCHER_STALL_REAP_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultStallReapGrace
}

// reconcileStalled cancels any run whose LastEventAt is older than the
// configured stall timeout. The dispatch goroutine will eventually post
// cmdRunFinished with context.Canceled, which schedules the retry.
// If the worker still hasn't returned stallReapGrace after the initial
// cancel, we force-reap the slot to keep dispatcher concurrency healthy
// — see refreshRunningStates for the same pattern in the
// tracker-disappeared case.
func (c *Dispatcher) reconcileStalled(ctx context.Context, cfg *Config) {
	timeout := cfg.StallTimeout()
	if timeout <= 0 {
		return
	}
	now := time.Now()
	// Snapshot ids so finishRun's delete(c.state.running, id) doesn't
	// invalidate the range we're walking.
	type stalledRow struct {
		id string
		r  *runningEntry
	}
	var rows []stalledRow
	for id, r := range c.state.running {
		// lastEventTime() returns the freshest of the actor-applied
		// LastEventAt and the synchronously-updated atomic heartbeat.
		// The atomic prevents false stall when cmdEvents are queued
		// in c.cmds behind a slow tick (was a 2026-05-21 dogfood bug).
		if now.Sub(r.lastEventTime()) <= timeout {
			continue
		}
		rows = append(rows, stalledRow{id, r})
	}
	for _, row := range rows {
		id, r := row.id, row.r
		if r.CancelIssuedAt.IsZero() {
			c.logger.Warn("dispatcher: %s stalled (no event for %s) — cancelling", r.Identifier, now.Sub(r.LastEventAt))
			if r.Cancel != nil {
				r.Cancel()
			}
			r.CancelIssuedAt = now
			continue
		}
		if now.Sub(r.CancelIssuedAt) <= resolveStallReapGrace() {
			c.logger.Debug("dispatcher: %s still draining (cancel issued %s ago)", r.Identifier, now.Sub(r.CancelIssuedAt))
			continue
		}
		c.logger.Warn("dispatcher: %s worker not exiting %s after cancel — force-reaping slot", r.Identifier, now.Sub(r.CancelIssuedAt))
		if r.Cancel != nil {
			r.Cancel()
		}
		c.state.tombstones[id] = struct{}{}
		c.finishRun(ctx, id, context.Canceled)
	}
}

// refreshRunningStates queries the tracker for each running issue and
// cancels any whose state moved out of the eligible set externally
// (operator closed it, blocker added, …).
func (c *Dispatcher) refreshRunningStates(ctx context.Context) {
	if len(c.state.running) == 0 {
		return
	}
	ids := make([]string, 0, len(c.state.running))
	for id := range c.state.running {
		ids = append(ids, id)
	}
	states, err := c.tracker.RefreshStates(ctx, ids)
	if err != nil {
		c.logger.Warn("dispatcher: tracker RefreshStates: %v", err)
		return
	}
	for _, id := range ids {
		r := c.state.running[id]
		newState, ok := states[id]
		if !ok {
			c.logger.Info("dispatcher: %s disappeared from tracker — cancelling", r.Identifier)
			if r.Cancel != nil {
				r.Cancel()
			}
			// Reap the slot immediately. A worker that swallows ctx
			// cancellation (some claude_code subprocesses ignore
			// SIGINT) would otherwise hold the slot until process
			// exit, gradually starving max_concurrent. finishRun is
			// idempotent, so if the worker later posts cmdRunFinished
			// the duplicate is a no-op.
			//
			// Plant a tombstone so isClaimed keeps blocking re-dispatch
			// until the worker actually exits. Without it, the next
			// tick could grant the slot to a sibling dispatch that
			// lands on the same workspace before the original worker
			// has released its file locks / mid-flight git operations.
			c.state.tombstones[id] = struct{}{}
			c.finishRun(ctx, id, context.Canceled)
			continue
		}
		if newState != r.WorkflowState {
			c.logger.Info("dispatcher: %s moved %s → %s externally — cancelling", r.Identifier, r.WorkflowState, newState)
			if r.Cancel != nil {
				r.Cancel()
			}
		}
	}
}

func (c *Dispatcher) hasSlot(state string, cfg *Config) bool {
	if len(c.state.running) >= cfg.Agent.MaxConcurrent {
		return false
	}
	if cap, ok := cfg.Agent.MaxConcurrentByState[state]; ok {
		if c.state.slotsByState[state] >= cap {
			return false
		}
	}
	return true
}

// dispatch claims the issue, allocates a workspace, and spawns the
// worker goroutine. Runs on the actor goroutine — must be fast.
func (c *Dispatcher) dispatch(ctx context.Context, iss tracker.Issue) {
	cfg := c.cfg.Load()
	if err := c.tracker.Claim(ctx, iss.ID, c.hostMarker); err != nil {
		if errors.Is(err, tracker.ErrClaimConflict) {
			c.logger.Info("dispatcher: %s already claimed elsewhere, skipping", iss.Identifier)
			return
		}
		c.logger.Warn("dispatcher: claim %s: %v", iss.Identifier, err)
		return
	}

	wsPath, created, err := c.workspaces.Create(iss.ID)
	if err != nil {
		c.logger.Warn("dispatcher: workspace create %s: %v", iss.Identifier, err)
		_ = c.tracker.Release(ctx, iss.ID, c.hostMarker)
		return
	}

	attempt := 0
	if cur, ok := c.state.retries[iss.ID]; ok {
		attempt = cur.Attempt
		// The retry entry has done its job — surrender it now so the
		// new runningEntry is the sole bookkeeping. (cmdRetryDue
		// already stopped the timer when it fired.)
		if cur.Timer != nil {
			cur.Timer.Stop()
		}
		delete(c.state.retries, iss.ID)
	}
	runID := newRunID(iss.ID, attempt)
	runCtx, cancel := context.WithCancel(ctx)

	entry := &runningEntry{
		IssueID:       iss.ID,
		Identifier:    iss.Identifier,
		RunID:         runID,
		WorkflowState: iss.WorkflowState,
		WorkspacePath: wsPath,
		StartedAt:     time.Now().UTC(),
		LastEventAt:   time.Now().UTC(),
		Attempt:       attempt,
		Cancel:        cancel,
		issueSnapshot: iss,
	}
	entry.touchEvent(time.Now())
	c.state.running[iss.ID] = entry
	c.state.slotsByState[iss.WorkflowState]++

	spec := c.buildSpec(cfg, iss, runID, wsPath, attempt, entry)

	c.logger.Info("dispatcher: dispatching %s → run=%s (attempt=%d, workspace=%s)", iss.Identifier, runID, attempt, wsPath)

	c.workersWG.Add(1)
	go func() {
		defer c.workersWG.Done()
		c.runWorker(runCtx, entry, created, spec)
	}()
}

func (c *Dispatcher) buildSpec(cfg *Config, iss tracker.Issue, runID, wsPath string, attempt int, entry *runningEntry) DispatchSpec {
	tplCtx := TemplateContext{
		Issue: iss,
		Dispatcher: DispatcherVars{
			Name:          cfg.Name,
			RunID:         runID,
			WorkspacePath: wsPath,
			Attempt:       attempt,
		},
	}
	// Per-assignee overrides win wholesale: when a bot has its own
	// AssigneeDispatch entry, its var/attachment map replaces the global
	// one rather than merging. This keeps each bot's input contract
	// explicit — operators see exactly what they bind.
	dc := cfg.Dispatch
	if iss.Assignee != "" {
		if ov, ok := cfg.AssigneeDispatch[iss.Assignee]; ok {
			dc = ov
		}
	}
	vars := map[string]any{}
	for k, src := range dc.Vars {
		tpl, err := ParseTemplate(src)
		if err != nil {
			c.logger.Warn("dispatcher: dispatch.vars[%s]: %v", k, err)
			continue
		}
		v, err := tpl.Render(tplCtx)
		if err != nil {
			c.logger.Warn("dispatcher: render dispatch.vars[%s]: %v", k, err)
			continue
		}
		vars[k] = v
	}
	attachments := map[string]any{}
	for k, src := range dc.Attachments {
		tpl, err := ParseTemplate(src)
		if err != nil {
			c.logger.Warn("dispatcher: dispatch.attachments[%s]: %v", k, err)
			continue
		}
		v, err := tpl.Render(tplCtx)
		if err != nil {
			c.logger.Warn("dispatcher: render dispatch.attachments[%s]: %v", k, err)
			continue
		}
		attachments[k] = v
	}
	// Per-ticket bot + BotArgs overrides — set on the issue itself,
	// resolved against cfg.Bots.Paths. Order:
	//   1. Per-ticket Bot replaces the config workflow path entirely.
	//      If we can't resolve, fall back to cfg.Workflow + log loudly:
	//      a stale name on a ticket should not silently halt dispatch
	//      (operators may have renamed/moved a bot mid-flight).
	//   2. Per-ticket BotArgs merge over the rendered vars key-by-key,
	//      with iss.BotArgs winning for declared keys. Keys not in the
	//      workflow's vars schema get a warn log but are still passed
	//      through (the engine will surface its own diagnostic).
	workflowPath := cfg.Workflow
	if iss.Bot != "" {
		resolved, err := botregistry.ResolveBotPath(iss.Bot, cfg.Bots.Paths)
		if err != nil {
			c.logger.Warn("dispatcher: resolve bot %q for issue %s: %v — falling back to config.workflow", iss.Bot, iss.ID, err)
		} else {
			workflowPath = resolved
		}
	}
	for k, v := range iss.BotArgs {
		vars[k] = v
	}
	return DispatchSpec{
		WorkflowPath:  workflowPath,
		RunID:         runID,
		WorkspacePath: wsPath,
		StoreDir:      c.storeDir,
		Vars:          vars,
		Attachments:   attachments,
		Assignee:      iss.Assignee,
		OnEvent: func(name string) {
			// Synchronous, lock-free heartbeat: read by reconcileStalled
			// without needing the actor to drain c.cmds first. This is
			// the load-bearing fix for the false-positive stall observed
			// in the 2026-05-21 dogfood, where tick() ran reconcileStalled
			// before applying queued cmdEvent updates and cancelled a
			// healthy actively-progressing run at the 10min mark.
			if entry != nil {
				entry.touchEvent(time.Now())
			}
			// cmdEvent still posts so the actor's LastEventName /
			// snapshot rendering stay accurate. Non-blocking now: if
			// the channel is full we drop the observability message;
			// the atomic heartbeat above already protects stall safety.
			select {
			case c.cmds <- cmdEvent{issueID: iss.ID, eventName: name}:
			case <-c.stop:
			default:
			}
		},
	}
}

// runWorker is the dispatch goroutine. Runs all hooks and the workflow,
// then posts cmdRunFinished. Hook failures fail the run.
//
// All sends on c.cmds are guarded by c.stop so a shutdown that races
// a late-finishing worker doesn't leak a goroutine blocked forever on
// a full channel.
func (c *Dispatcher) runWorker(ctx context.Context, entry *runningEntry, created bool, spec DispatchSpec) {
	env := c.dispatchEnv(entry, spec)

	// Snapshot the hooks struct from the atomic config pointer once,
	// at the start of the worker. A mid-flight reload doesn't suddenly
	// swap callback bodies, and the read is guaranteed consistent
	// across the three Run invocations below (F-CD-10).
	hooks := c.cfg.Load().Hooks

	if created && hooks.AfterCreate != nil {
		if err := hooks.AfterCreate.Run(ctx, c.logger, "after_create", entry.WorkspacePath, env); err != nil {
			c.postFinished(entry.IssueID, fmt.Errorf("after_create hook: %w", err))
			return
		}
	}
	if err := hooks.BeforeRun.Run(ctx, c.logger, "before_run", entry.WorkspacePath, env); err != nil {
		c.postFinished(entry.IssueID, fmt.Errorf("before_run hook: %w", err))
		return
	}

	dispatchErr := c.runner.Dispatch(ctx, spec)

	// after_run is best-effort: log failures but don't override the
	// dispatch result.
	if err := hooks.AfterRun.Run(ctx, c.logger, "after_run", entry.WorkspacePath, env); err != nil {
		c.logger.Warn("dispatcher: after_run hook for %s: %v", entry.Identifier, err)
	}

	c.postFinished(entry.IssueID, dispatchErr)
}

func (c *Dispatcher) postFinished(issueID string, err error) {
	select {
	case c.cmds <- cmdRunFinished{issueID: issueID, err: err}:
	case <-c.stop:
	}
}

func (c *Dispatcher) dispatchEnv(entry *runningEntry, spec DispatchSpec) []string {
	env := []string{
		"ITERION_ISSUE_ID=" + entry.IssueID,
		"ITERION_ISSUE_IDENTIFIER=" + entry.Identifier,
		"ITERION_ISSUE_STATE=" + entry.WorkflowState,
		"ITERION_RUN_ID=" + spec.RunID,
		"ITERION_WORKSPACE=" + spec.WorkspacePath,
	}
	if spec.StoreDir != "" {
		env = append(env, "ITERION_STORE_DIR="+spec.StoreDir)
	}
	return env
}

// newRunID produces a deterministic-ish, sortable run ID for dispatch.
func newRunID(issueID string, attempt int) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			return r
		}
		return '_'
	}, issueID)
	return fmt.Sprintf("dispatcher-%s-%d-%d", clean, attempt, time.Now().UnixMilli())
}
