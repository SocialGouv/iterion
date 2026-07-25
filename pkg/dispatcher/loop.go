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
	"github.com/SocialGouv/iterion/pkg/clock"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
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
	c.state.lastTickAt = time.Now().UTC()

	c.reconcileStalled(ctx, cfg)
	c.refreshRunningStates(ctx)
	c.reconcileParked(ctx)

	if c.paused.Load() {
		c.fireSnapshot()
		return
	}

	// Daily spend cap: recompute the day's spend status and, when the cap
	// is reached (and not overridden for the day), stop launching new
	// work. Runs already in flight pause themselves at their next node
	// boundary via the shared <store>/spend/ ledger; the gate here only
	// suppresses new dispatches. The cap limit is read fresh from the
	// hot-reloadable config so a reload takes effect on the next tick.
	c.refreshCostCap(ctx, cfg)
	if cc := c.state.costCap; cc != nil && cc.Exceeded {
		c.fireSnapshot()
		return
	}

	// Skip the tracker call when we're already at the global cap —
	// any candidate we'd see is unactionable. Keeping the actor
	// responsive for cmdRunFinished / cmdEvent matters more than
	// discovering candidates we can't dispatch. Once an in-flight run
	// finishes the next tick will pick up the work.
	if len(c.state.running) >= cfg.Agent.MaxConcurrent {
		c.fireSnapshot()
		return
	}

	// Discovery (tracker.ListCandidates) is the slowest step a poll makes
	// — external HTTP for github/forgejo. Run it OFF the actor goroutine
	// (ADR-028 Step 2): a side goroutine does the I/O and posts the result
	// back as cmdCandidates, where the actor runs the fast in-memory
	// sort/prune/dispatch logic. While discovery is in flight the actor
	// keeps draining cmdRunFinished / cmdEvent / cmdCancel instead of
	// parking inside the HTTP call.
	//
	// Single-flight: skip launching a second discovery while one is still
	// running. The in-flight one will post its candidates and the next
	// tick re-evaluates. discoveryInFlight is set/cleared on the actor
	// (here + cmdCandidates.apply), so no atomic is needed.
	if c.state.discoveryInFlight {
		c.fireSnapshot()
		return
	}
	c.state.discoveryInFlight = true
	c.launchDiscovery(ctx)
	c.fireSnapshot()
}

// launchDiscovery runs tracker.ListCandidates on a short-lived goroutine
// OFF the actor and posts the result back as cmdCandidates. It MUST NOT
// read or write c.state (running, slots, claims, retries, costCap) — those
// are actor-only. It only reads cfg via the atomic pointer (indirectly,
// through the tracker) and calls the tracker. Single-flight is guaranteed
// by the actor's c.state.discoveryInFlight guard in tick().
//
// Tracked on workersWG so Stop() drains it; the send is guarded by c.stop
// so a discovery that finishes after the actor has exited never leaks on a
// blocked channel send. See ADR-028 Step 2.
func (c *Dispatcher) launchDiscovery(ctx context.Context) {
	c.workersWG.Add(1)
	go func() {
		defer c.workersWG.Done()
		defer func() {
			if r := recover(); r != nil {
				err := fmt.Errorf("panic in tracker ListCandidates: %v", r)
				c.logger.Error("dispatcher: %v", err)
				c.postCmd(cmdCandidates{err: err})
			}
		}()
		issues, err := c.tracker.ListCandidates(ctx)
		c.postCmd(cmdCandidates{issues: issues, err: err})
	}()
}

// refreshCostCap recomputes the daily spend-cap status into state.costCap
// (nil when the cap is disabled or no ledger store is wired). Runs on the
// actor goroutine. Errors are logged and leave the prior status in place
// so a transient ledger read failure doesn't flip the gate open/closed.
func (c *Dispatcher) refreshCostCap(ctx context.Context, cfg *Config) {
	g := c.newDailyCapGuard(cfg)
	if g == nil {
		c.state.costCap = nil
		return
	}
	st, err := g.Status(ctx)
	if err != nil {
		c.logger.Warn("dispatcher: daily spend cap status: %v", err)
		return
	}
	c.state.costCap = &CostCapView{
		Date:           st.Date,
		SpentUSD:       st.SpentUSD,
		LimitUSD:       st.LimitUSD,
		Exceeded:       st.Exceeded,
		OverrideActive: st.OverrideActive,
	}
}

// newDailyCapGuard builds a spend-cap guard from the dispatcher's
// SINGLETON SpendStore and the hot-reloadable limit, or nil when the cap
// is disabled (limit <= 0 or no ledger store). Reusing the single shared
// c.spendStore across every dispatched run is load-bearing: all ledger
// read-modify-writes then serialise on one store mutex, so concurrent
// runs can't clobber each other's runs_contributed entry — unlike a
// per-run store.New() (the engine opens one for run.json/events) which
// would race on <store>/spend/<day>.json. NewDailyCapGuard already
// returns nil for a nil store or non-positive limit.
func (c *Dispatcher) newDailyCapGuard(cfg *Config) *runtime.DailyCapGuard {
	return runtime.NewDailyCapGuard(
		c.spendStore,
		clock.Default,
		runtime.DailyCapConfig{MaxCostPerDayUSD: cfg.Limits.MaxCostPerDayUSD},
	)
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
		// A run still in its claimed→running setup hasn't started — don't
		// stall-reap it on its claim-time watermark (ADR-028 Step 4). The
		// setup worker will report back (cmdDispatchSetupDone) or fail
		// (cmdRunFinished) promptly; the regular path handles it from there.
		if r.setupPending {
			continue
		}
		if r.CancelIssuedAt.IsZero() {
			atomicLag := now.Sub(r.lastEventTime())
			actorLag := now.Sub(r.LastEventAt)
			c.logger.Warn("dispatcher: %s stalled (atomic_lag=%s actor_lag=%s timeout=%s) — cancelling", r.Identifier, atomicLag, actorLag, timeout)
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
	cfg := c.cfg.Load()
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
		// Skip entries still in their claimed→running setup (ADR-028 Step 4).
		// The in-progress transition's UpdateState runs off the actor, so the
		// tracker may already read RunningState while the entry has not yet
		// recorded TransitionedFromState — comparing now would misread our own
		// in-flight move as an external one and spuriously cancel the run. The
		// next tick re-evaluates once cmdDispatchSetupDone has landed.
		if r.setupPending {
			continue
		}
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
		// The dispatcher's own in-progress transition (see dispatch
		// and Agent.RunningState) moves the tracker state from
		// r.WorkflowState (the snapshot at claim time) to
		// cfg.Agent.RunningState. That move is OURS — not an
		// external operator action — so it must not trigger a cancel
		// here. r.TransitionedFromState is the source state we
		// transitioned from; when it is non-empty, the expected
		// in-flight state is the running_state, not the original
		// WorkflowState snapshot.
		expected := r.WorkflowState
		if r.TransitionedFromState != "" {
			expected = cfg.Agent.RunningState
		}
		if newState != expected {
			c.logger.Info("dispatcher: %s moved %s → %s externally — cancelling", r.Identifier, expected, newState)
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
//
// The three phases are extracted into helpers so each can be read on
// its own without scrolling through the others:
//   - resolveExplicitBot: honest-fail on an unresolvable / unrouted Bot
//   - resolveRunID:       retry-vs-fresh runID resolution (post-claim)
//   - buildRunningEntry:  the slot+entry allocation + bookkeeping
//
// The claim itself stays inline — it is the conflict authority and the
// boundary between "skip safely" and "we own this issue now".
func (c *Dispatcher) dispatch(ctx context.Context, iss tracker.Issue) {
	cfg := c.cfg.Load()

	// Deterministic ticket router (opt-in): route an UNASSIGNED new issue by
	// whether a PR already links it — a linked PR means the inbound PR-webhook
	// owns the work (branch-improve on the PR branch), so the dispatcher steps
	// aside to avoid a second, branch-less (empty-diff) run (the ticket<->PR
	// dedup); no linked PR routes to the implement bot (Featurly). An explicit
	// bot/assignee always wins — the router only touches fully-unassigned issues.
	if cfg.TicketRouter.Enabled && iss.Bot == "" && iss.Assignee == "" {
		bot, ok := c.routeUnassignedIssue(ctx, cfg, iss)
		if !ok {
			return // a PR owns this issue; skip recorded, PR-webhook runs Billy
		}
		iss.Bot = bot
	}

	if !c.resolveExplicitBot(cfg, iss) {
		return
	}
	// Past the bot guards: this issue is dispatchable, so drop any skip
	// entry a prior tick recorded (the operator fixed the bot name or
	// added the route). Cleared here rather than on claim so a downstream
	// claim/workspace failure still reflects "no longer a routing skip".
	delete(c.state.dispatchSkips, iss.ID)

	// Claim stays ATOMIC on the actor — it is the conflict authority. A
	// conflict (or any claim error) skips the issue with NO slot allocated and
	// NO setup worker launched; the post-claim I/O offload below only runs
	// AFTER a confirmed claim. See ADR-028 Step 4.
	//
	// Journal BEFORE Claim: a crash between the two leaves an entry the
	// boot sweep resolves with an idempotent Release; the reverse order
	// would leave an unjournalled live claim — exactly the stranding the
	// journal exists to prevent.
	c.claims.Record(claimEntry{IssueID: iss.ID, Identifier: iss.Identifier, Marker: c.hostMarker, ClaimedAt: time.Now().UTC()})
	if err := c.tracker.Claim(ctx, iss.ID, c.hostMarker); err != nil {
		c.claims.Remove(iss.ID)
		if errors.Is(err, tracker.ErrClaimConflict) {
			c.logger.Info("dispatcher: %s already claimed elsewhere, skipping", iss.Identifier)
			return
		}
		c.logger.Warn("dispatcher: claim %s: %v", iss.Identifier, err)
		return
	}

	runID, resumeFromRunID, workspaceGeneration, attempt, ok := c.resolveRunID(ctx, iss)
	if !ok {
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	entry := c.buildRunningEntry(iss, runID, workspaceGeneration, attempt, cancel)

	spec := c.buildSpec(cfg, iss, runID, entry.WorkspacePath, attempt, entry)
	spec.ResumeFromRunID = resumeFromRunID

	if resumeFromRunID != "" {
		c.logger.Info("dispatcher: resuming %s → run=%s (attempt=%d, workspace=%s)", iss.Identifier, runID, attempt, entry.WorkspacePath)
	} else {
		c.logger.Info("dispatcher: dispatching %s → run=%s (attempt=%d, workspace=%s)", iss.Identifier, runID, attempt, entry.WorkspacePath)
	}

	// Offload the post-claim setup I/O (in-progress UpdateState +
	// workspaces.Create) off the actor, then continue into the run worker on
	// the same goroutine. The actor returns here immediately, staying
	// responsive while the (potentially slow) tracker transition runs.
	c.launchDispatchSetup(dispatchSetupPlan{
		issueID:             iss.ID,
		workspaceGeneration: workspaceGeneration,
		identifier:          iss.Identifier,
		sourceState:         iss.WorkflowState,
		runningTarget:       cfg.Agent.RunningState,
		runCtx:              runCtx,
		entry:               entry,
		spec:                spec,
	})
}

// resolveExplicitBot enforces honest-fail on explicit-bot resolution.
// When the issue names a specific bot (iss.Bot != "") that the registry
// can't resolve, do NOT claim + silently fall back to the default
// workflow: that runs an unrelated no-op (dispatcher_default's
// triage→done) and reports a misleading "success", leaving the ticket
// in review/done with nothing relevant done. Skip the dispatch instead
// — the issue stays eligible in its source state, so a transient scan
// failure (e.g. mid dev-server rebuild) recovers on the next tick, and
// a persistent one (typo'd / missing bot) stays visible via the warn
// rather than burning a default run every poll. buildSpec re-resolves
// for the happy path; this guard only gates the explicit-bot case.
//
// Returns false to signal "skip dispatch"; the caller must not claim.
// Runs on the actor goroutine (recordDispatchSkip touches c.state).
func (c *Dispatcher) resolveExplicitBot(cfg *Config, iss tracker.Issue) bool {
	if iss.Bot == "" {
		return true
	}
	if _, err := botregistry.ResolveBotPath(iss.Bot, cfg.Bots.Paths); err != nil {
		c.logger.Warn("dispatcher: %s names bot %q which can't be resolved: %v — skipping (refusing to silently run the default workflow); will retry next tick", iss.Identifier, iss.Bot, err)
		c.recordDispatchSkip(iss, fmt.Sprintf("bot %q can't be resolved (%v)", iss.Bot, err))
		return false
	}
	// The bot FILE resolves — but does it have an actual dispatch
	// route? Without one it would fall through to the default
	// workflow, running an unrelated bot with the wrong
	// structured-output schemas and reporting a misleading success.
	// Refuse, same as the unresolvable case. With registry-driven
	// routing, any ENABLED discovered bot has a route (no
	// assignee_workflows entry needed); a bot lands here only when it
	// is disabled in the catalog or — with no bots discovery
	// configured — absent from assignee_workflows. (RoutingRunner
	// implements HasRoute; a plain single-workflow runner doesn't —
	// there an explicit bot has no routing concept, so we don't gate
	// it and preserve the legacy single-workflow behaviour.)
	if rc, ok := c.runner.(interface{ HasRoute(string) bool }); ok && !rc.HasRoute(iss.Bot) {
		c.logger.Warn("dispatcher: %s names bot %q which resolves to a file but has no active dispatch route — skipping (refusing to silently run the default workflow). Likely disabled in the catalog (enable it in the studio Catalog manager) or, with no bots discovery configured, missing from assignee_workflows.", iss.Identifier, iss.Bot)
		c.recordDispatchSkip(iss, fmt.Sprintf("bot %q has no active dispatch route (disabled in catalog or unrouted)", iss.Bot))
		return false
	}
	return true
}

// linkedPRProbe is an OPTIONAL tracker capability: report whether an issue is
// already linked by an open pull/merge request. A tracker that can't answer
// (native, forgejo today) is treated as "no PR", so the router never blocks an
// issue — it just can't dedup against a PR-webhook it cannot observe.
type linkedPRProbe interface {
	HasLinkedPR(ctx context.Context, id string) (bool, error)
}

// labelApplier is an OPTIONAL tracker capability: add a visible label. The
// router uses it best-effort for the bot:featurly / bot:billy association; a
// tracker without it just skips the label (routing is unaffected).
type labelApplier interface {
	ApplyLabel(ctx context.Context, id, label string) error
}

// routeUnassignedIssue is the deterministic ticket router. It returns
// (implementBot, true) for an issue with NO linked PR — Featurly implements it —
// or ("", false) to STEP ASIDE because a PR already owns the issue: the inbound
// PR-webhook runs the branch-improve bot on the PR branch, and dispatching it
// here would double-run on an empty diff (an issue carries no PR branch). A
// best-effort visible label records the bot:featurly / bot:billy association.
// A tracker that can't answer "linked PR?" degrades to the implement bot (never
// blocks, never dedups against a PR-webhook it can't see). Runs on the actor
// goroutine; the probe blocks it briefly (acceptable for the MVP — a follow-up
// can offload it like launchDiscovery).
func (c *Dispatcher) routeUnassignedIssue(ctx context.Context, cfg *Config, iss tracker.Issue) (string, bool) {
	implementBot := cfg.TicketRouter.ImplementBotOrDefault()
	probe, ok := c.tracker.(linkedPRProbe)
	if !ok {
		return implementBot, true // can't check PRs → implement (safe default)
	}
	hasPR, err := probe.HasLinkedPR(ctx, iss.ID)
	if err != nil {
		c.logger.Warn("dispatcher: PR-link probe for %s failed: %v — routing to %s", iss.Identifier, err, implementBot)
		return implementBot, true
	}
	if hasPR {
		c.applyBotLabelBestEffort(ctx, iss.ID, "bot:billy")
		c.recordDispatchSkip(iss, "a linked PR owns this issue — the PR-webhook runs the branch-improve bot on the PR branch; the dispatcher steps aside to avoid a double run")
		c.logger.Info("dispatcher: %s has a linked PR — stepping aside to the PR-webhook (branch-improve on the PR)", iss.Identifier)
		return "", false
	}
	c.applyBotLabelBestEffort(ctx, iss.ID, "bot:featurly")
	return implementBot, true
}

// applyBotLabelBestEffort stamps a visible bot:* label when the tracker supports
// it. Non-fatal: a label failure never blocks the routing decision.
func (c *Dispatcher) applyBotLabelBestEffort(ctx context.Context, id, label string) {
	la, ok := c.tracker.(labelApplier)
	if !ok {
		return
	}
	if err := la.ApplyLabel(ctx, id, label); err != nil {
		c.logger.Warn("dispatcher: apply label %q to %s: %v (non-fatal)", label, id, err)
	}
}

// resolveRunID picks the runID (resumed vs freshly minted) and the
// retry attempt for an issue we just claimed. Runs on the actor
// goroutine (reads + mutates c.state.retries). Returns ok=false when a
// fresh runID can't be minted — in that case the claim is released
// inline and the caller must abort dispatch immediately, before
// allocating a slot. See ADR-028 Step 4.
//
// Returns:
//   - runID:           the run id to use (resume target or freshly minted)
//   - resumeFromRunID: non-empty when the engine should resume from
//     checkpoint instead of starting fresh (passed through as
//     DispatchSpec.ResumeFromRunID)
//   - attempt:         the retry attempt number (0 for the first try)
func (c *Dispatcher) resolveRunID(ctx context.Context, iss tracker.Issue) (
	runID, resumeFromRunID, workspaceGeneration string,
	attempt int,
	ok bool,
) {
	// hadRetryEntry records whether this dispatch is servicing a
	// scheduled retry (an in-memory retryEntry existed). It gates the
	// cross-restart fallback below: a retry entry carries a DELIBERATE
	// resume-vs-fresh decision, and an empty PrevRunID on it means "run
	// fresh" — e.g. scheduleRetry dropped a doomed resume because the bot
	// source changed. The persisted last_run pointer must not override
	// that decision.
	hadRetryEntry := false
	if cur, present := c.state.retries[iss.ID]; present {
		hadRetryEntry = true
		attempt = cur.Attempt
		// PrevRunID is set on the retry entry iff the prior run's
		// status was resumable (failed_resumable / cancelled /
		// paused_operator) — see scheduleRetry. We pass it through so
		// the engine resumes from checkpoint instead of re-executing
		// every upstream node. A clean retry (PrevRunID empty) falls
		// through to GenerateRunID below.
		resumeFromRunID = cur.PrevRunID
		workspaceGeneration = cur.WorkspaceGeneration
		// The retry entry has done its job — surrender it now so the
		// new runningEntry is the sole bookkeeping. (cmdRetryDue
		// already stopped the timer when it fired.)
		if cur.Timer != nil {
			cur.Timer.Stop()
		}
		delete(c.state.retries, iss.ID)
	}
	// Cross-restart fallback: when no in-memory retry entry exists
	// (daemon was restarted, or the dispatcher is picking up an
	// orphaned ticket), consult the tracker's persisted "last run"
	// pointer. Only the native tracker exposes this lookup today;
	// other adapters silently fall through to a fresh runID.
	//
	// Gated on !hadRetryEntry: when a retry entry WAS present its
	// PrevRunID is authoritative — including an empty value meaning
	// "scheduleRetry deliberately dropped a doomed resume (bot source
	// changed since the prior run) — run fresh". Without this guard the
	// fallback re-reads the persisted last_run_id (still failed_resumable
	// on disk) and re-resumes the same source-changed run on the very
	// next poll, an infinite resume→fail→revert loop that strands the
	// ticket. scheduleRetry's in-memory drop alone can't survive this
	// re-resolution. See pkg/dispatcher/retry.go isResumeSourceChanged.
	if resumeFromRunID == "" && !hadRetryEntry {
		type lastRunLookup interface {
			LastRunForIssue(id string) (string, error)
		}
		if look, lookOK := c.tracker.(lastRunLookup); lookOK {
			if prev, err := look.LastRunForIssue(iss.ID); err == nil {
				resumeFromRunID = c.resumableRunID(prev)
			}
		}
	}
	if resumeFromRunID != "" {
		if workspaceGeneration == "" {
			workspaceGeneration = resumeFromRunID
		}
		return resumeFromRunID, resumeFromRunID, workspaceGeneration, attempt, true
	}
	freshID, err := store.GenerateRunID()
	if err != nil {
		c.logger.Warn("dispatcher: mint run id for %s: %v", iss.Identifier, err)
		// Nothing was transitioned and no slot was allocated yet (the
		// post-claim setup I/O runs off the actor below, only after the
		// entry is in place) — just release the claim. See ADR-028 Step 4.
		_ = c.tracker.Release(ctx, iss.ID, c.hostMarker)
		return "", "", "", attempt, false
	}
	if workspaceGeneration == "" {
		workspaceGeneration = freshID
	}
	return freshID, "", workspaceGeneration, attempt, true
}

// buildRunningEntry constructs and registers the runningEntry that
// dispatch hands off to the off-actor setup worker (ADR-028 Step 4).
// Runs on the actor goroutine — it mutates c.state.running and
// c.state.slotsByState and stamps last_run, so MUST stay actor-local.
//
// The slot is allocated IMMEDIATELY after the confirmed claim with
// setupPending=true and TransitionedFromState not-yet-known. This keeps
// MaxConcurrent / per-state caps enforced from claim time (no
// over-dispatch window while the setup I/O is in flight) and makes
// isClaimed/the dispatch loop skip the issue. The off-actor setup
// worker fills TransitionedFromState via cmdDispatchSetupDone once the
// in-progress transition has run.
func (c *Dispatcher) buildRunningEntry(
	iss tracker.Issue,
	runID, workspaceGeneration string,
	attempt int,
	cancel context.CancelFunc,
) *runningEntry {
	// The workspace path is deterministic from the issue + run generation
	// (Workspaces.PathForRun == the directory CreateForRun materialises), so it
	// is known here on the actor even though mkdir runs off-actor. Never reusing
	// a completed run's absolute path prevents a stale background writer from
	// contaminating a later dispatch of the same issue.
	wsPath := c.workspaces.PathForRun(iss.ID, workspaceGeneration)
	now := time.Now().UTC()
	entry := &runningEntry{
		IssueID:             iss.ID,
		Identifier:          iss.Identifier,
		RunID:               runID,
		WorkspaceGeneration: workspaceGeneration,
		WorkflowState:       iss.WorkflowState,
		WorkspacePath:       wsPath,
		StartedAt:           now,
		LastEventAt:         now,
		Attempt:             attempt,
		Cancel:              cancel,
		issueSnapshot:       iss,
		setupPending:        true,
	}
	entry.touchEvent(time.Now())
	c.state.running[iss.ID] = entry
	c.state.slotsByState[iss.WorkflowState]++

	// Stamp last_run at dispatch (not just at finish) so the studio's
	// IssueModal / WatchPanel "run ↗" link is live for the whole run
	// instead of pointing at the previous run until this one completes.
	// run.json doesn't exist yet, so stampLastRun falls back to the
	// workspace path; the finish-time stamp later upgrades it to the
	// resolved worktree path.
	c.stampLastRun(iss.ID, entry)
	// A fresh dispatch (incl. a re-dispatch of a previously-parked issue)
	// supersedes any prior pause — clear the denormalized awaiting-input
	// badge so the card doesn't show a stale ⏸ while the new run executes.
	c.setAwaitingInput(iss.ID, false)
	return entry
}

// dispatchSetupPlan is the value-copy of everything the off-actor dispatch
// setup worker (runDispatchSetup) needs. Following the ADR-028 Step 2/3
// anti-race boundary, the SETUP portion (UpdateState + workspaces.Create) reads
// ONLY these immutable fields plus dispatcher-immutables (c.tracker,
// c.workspaces, c.logger) — never c.state or c.cfg. cfg-derived inputs
// (runningTarget) are captured on the actor here so a mid-flight Reload can't
// split the decision. entry/spec are carried through solely to hand to
// runWorker AFTER setup; the setup portion never touches them.
type dispatchSetupPlan struct {
	issueID             string
	workspaceGeneration string
	identifier          string
	sourceState         string // iss.WorkflowState at claim time (the transition source)
	runningTarget       string // cfg.Agent.RunningState snapshot at claim time
	runCtx              context.Context
	entry               *runningEntry
	spec                DispatchSpec
}

// launchDispatchSetup runs the post-claim dispatch setup OFF the actor and,
// on success, continues into the run worker on the same goroutine. Tracked on
// workersWG so Stop() drains it (covering both the setup I/O and the run).
// Mirrors launchDiscovery / launchFinish. See ADR-028 Step 4.
func (c *Dispatcher) launchDispatchSetup(plan dispatchSetupPlan) {
	c.workersWG.Add(1)
	go func() {
		defer c.workersWG.Done()
		created, ok := c.runDispatchSetup(plan)
		if !ok {
			// Setup failed (or panicked): runDispatchSetup already posted
			// cmdDispatchSetupDone{err}, whose apply reuses finishRun's
			// teardown (slot free + transition revert + claim release +
			// retry). Nothing more to do — do NOT start the run.
			return
		}
		c.runWorker(plan.runCtx, plan.entry, created, plan.spec)
	}()
}

// runDispatchSetup performs the in-progress transition (best-effort) and the
// workspace mkdir off the actor, posting the result back via
// cmdDispatchSetupDone. Returns (created, ok); ok=false means setup failed and
// the run must NOT start (the failure command already drove finishRun). The
// recover guards ONLY the setup I/O — runWorker is invoked by the caller
// OUTSIDE it, preserving its existing panic semantics.
func (c *Dispatcher) runDispatchSetup(plan dispatchSetupPlan) (created bool, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("panic in dispatch setup for %s: %v", plan.identifier, r)
			c.logger.Error("dispatcher: %v", err)
			c.postCmd(cmdDispatchSetupDone{issueID: plan.issueID, err: err})
			created, ok = false, false
		}
	}()

	// In-progress transition: best-effort move out of the issue's source state
	// (typically "ready") into cfg.Agent.RunningState (typically "in_progress")
	// so the kanban surfaces in-flight work. Skipped when running_state is
	// disabled or the issue is already in the target state. Failures don't abort
	// dispatch — the claim is already taken and a stuck UpdateState shouldn't
	// strand the work. transitionedFrom records the source state IFF the move
	// succeeded; the rollback paths and finishRun read it (via the entry) to
	// revert. Moved off the actor in ADR-028 Step 4.
	var transitionedFrom string
	if target := plan.runningTarget; target != "" && plan.sourceState != target {
		if err := c.tracker.UpdateState(plan.runCtx, plan.issueID, target); err != nil {
			if !errors.Is(err, tracker.ErrTransitionRejected) && !errors.Is(err, tracker.ErrNotSupported) {
				c.logger.Warn("dispatcher: in-progress transition %s: %v", plan.identifier, err)
			}
			// continue regardless — claim is already taken.
		} else {
			transitionedFrom = plan.sourceState
		}
	}

	_, created, err := c.workspaces.CreateForRun(plan.issueID, plan.workspaceGeneration)
	if err != nil {
		c.logger.Warn("dispatcher: workspace create %s: %v", plan.identifier, err)
		// Carry transitionedFrom so the actor records it on the entry BEFORE
		// finishRun's revert reads it — then reuse the Step-3 teardown rather
		// than duplicating slot-free + revert + release + retry here.
		c.postCmd(cmdDispatchSetupDone{issueID: plan.issueID, transitionedFrom: transitionedFrom, err: err})
		return false, false
	}

	// Setup confirmed: hand the transition source back to the actor (which
	// records it + clears setupPending), then the caller runs the worker.
	c.postCmd(cmdDispatchSetupDone{issueID: plan.issueID, transitionedFrom: transitionedFrom})
	return created, true
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
	// The routing key selects BOTH the workflow and the per-assignee
	// dispatch overrides, so the two can't diverge. RoutingRunner picks the
	// pre-compiled workflow by spec.Assignee (ByAssignee is keyed by bot /
	// assignee name); Bot is the explicit dispatch directive and wins over
	// Assignee when set. A ticket that names a Bot but no Assignee would
	// otherwise fall through to the default workflow and never run its bot.
	// The bot FILE is resolved (and route-checked) by the guard at the top
	// of dispatch(); buildSpec carries no workflow path — the engine runs
	// its pre-compiled IR.
	routeAssignee := iss.Assignee
	if iss.Bot != "" {
		routeAssignee = iss.Bot
	}
	// Per-assignee overrides win wholesale: when a bot has its own
	// AssigneeDispatch entry, its var/attachment map replaces the global
	// one rather than merging. Keyed by routeAssignee (not raw iss.Assignee)
	// so a per-ticket Bot override binds THAT bot's inputs — otherwise an
	// issue with Assignee=X + Bot=Y ran Y's workflow with X's var bindings.
	dc := cfg.Dispatch
	if routeAssignee != "" {
		if ov, ok := cfg.AssigneeDispatch[routeAssignee]; ok {
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
	// NOTE: dispatch.attachments / assignee_dispatch[].attachments are
	// rejected at Config.Validate (see unsupportedAttachmentsErr), so dc
	// never carries any here — the dispatcher has no path to inject
	// per-issue attachments into a run. There is therefore deliberately
	// no attachments render block: a config that reached this point has
	// none. See docs/adr/013-dispatcher-attachments-unsupported.md.
	//
	// Per-ticket BotArgs merge over the rendered vars key-by-key, with
	// iss.BotArgs winning. A BotArgs key the routed workflow does not
	// declare as a var is silently dropped downstream — resolveVars
	// (pkg/runtime/engine.go) skips undeclared input keys with `continue`
	// and no log — so a typo'd / unknown bot_arg would otherwise reach the
	// bot as if unset, with no signal at all. Validate each key against the
	// routed workflow's declared vars (when the runner can report them) and
	// warn on a miss so the operator can reconcile why their override had no
	// effect. The studio's BotArgsForm flags the same keys at input time.
	declared := c.declaredVarsFor(routeAssignee)
	for k, v := range iss.BotArgs {
		vars[k] = v
		if declared != nil {
			if _, ok := declared[k]; !ok {
				c.logger.Warn("dispatcher: %s bot_arg %q is not a declared var of the routed workflow — it will be IGNORED at runtime (undeclared input vars are dropped). Fix the key to match the bot's vars schema or remove it.", iss.Identifier, k)
			}
		}
	}
	// feature_dev declares a required `feature_prompt` (no default) and reads
	// it in every prompt. A board ticket dispatched without
	// bot_args.feature_prompt would otherwise render the literal
	// "{{vars.feature_prompt}}" and produce a garbage run. Fall back to the
	// issue's own text so a plain ticket dispatched from the board / Nexie
	// Just Works. (Normalises the "feature-dev" hyphenated alias.)
	if b := strings.ToLower(strings.ReplaceAll(routeAssignee, "-", "_")); b == "feature_dev" {
		if s, _ := vars["feature_prompt"].(string); strings.TrimSpace(s) == "" {
			vars["feature_prompt"] = strings.TrimSpace(iss.Title + "\n\n" + iss.Body)
		}
	}
	return DispatchSpec{
		RunID:         runID,
		WorkspacePath: wsPath,
		StoreDir:      c.storeDir,
		Vars:          vars,
		// Built from the singleton SpendStore so this run records into —
		// and self-pauses against — the same ledger the refreshCostCap
		// gate reads. nil when the cap is disabled.
		DailyCap: c.newDailyCapGuard(cfg),
		Issue: &IssueRef{
			ID:         iss.ID,
			Identifier: iss.Identifier,
			Title:      iss.Title,
		},
		Assignee: routeAssignee,
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

// declaredVarsFor returns the set of var names declared by the workflow
// that routeKey will run, or nil when the runner can't report them (a
// test StubRunner, or a runner type that predates the DeclaredVars
// shape). A nil result makes buildSpec skip bot_arg validation rather
// than warn on everything — fail-open, since the validation is advisory.
func (c *Dispatcher) declaredVarsFor(routeKey string) map[string]struct{} {
	vd, ok := c.runner.(interface {
		DeclaredVars(string) map[string]struct{}
	})
	if !ok {
		return nil
	}
	return vd.DeclaredVars(routeKey)
}

// recordDispatchSkip notes that the actor refused to dispatch iss this
// scan because its explicit bot is unresolvable / unrouteable. Surfaced
// in the Snapshot so the board + dashboard can show WHY an eligible
// ticket is idle. Runs on the actor goroutine; pruned in tick() once the
// issue stops being a candidate and cleared in dispatch() once it claims.
func (c *Dispatcher) recordDispatchSkip(iss tracker.Issue, reason string) {
	if c.state.dispatchSkips == nil {
		c.state.dispatchSkips = map[string]DispatchSkipView{}
	}
	c.state.dispatchSkips[iss.ID] = DispatchSkipView{
		IssueID:    iss.ID,
		Identifier: iss.Identifier,
		Bot:        iss.Bot,
		Reason:     reason,
	}
}

// runWorker is the dispatch goroutine. Runs all hooks and the workflow,
// then posts cmdRunFinished. Hook failures fail the run.
//
// All sends on c.cmds are guarded by c.stop so a shutdown that races
// a late-finishing worker doesn't leak a goroutine blocked forever on
// a full channel.
func (c *Dispatcher) runWorker(ctx context.Context, entry *runningEntry, created bool, spec DispatchSpec) {
	// The ownership marker is created before after_create/before_run. Reuse its
	// external timestamp for the engine's nested worktree so a background
	// process spawned by either hook cannot predate and evade the runtime
	// process census. Missing/corrupt authority fails before any hook starts.
	workspaceGeneration := entryWorkspaceGeneration(entry)
	authoritySince, err := c.workspaces.AuthoritySinceForRun(entry.IssueID, workspaceGeneration)
	if err != nil {
		c.postFinished(entry.IssueID, fmt.Errorf("workspace process authority: %w", err))
		return
	}
	spec.WorktreeAuthoritySince = authoritySince
	env := c.dispatchEnv(entry, spec)

	// Snapshot the hooks struct from the atomic config pointer once,
	// at the start of the worker. A mid-flight reload doesn't suddenly
	// swap callback bodies, and the read is guaranteed consistent
	// across the four Run invocations below — after_create, before_run,
	// after_run, and the before_remove run inside cleanupWorkspace (F-CD-10).
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

	// On a clean finish, tear down the workspace per the persist policy —
	// first proving the persisted run safely accounts for its exact HEAD,
	// then running before_remove, re-proving the state, and deregistering
	// the linked worktree through runtime's lock-protected cleanup. Done
	// here on the worker goroutine (never the actor, where a shell hook
	// would freeze polling/dispatch/snapshots) and BEFORE
	// postFinished, so teardown completes before the actor releases the
	// claim and the issue becomes re-dispatchable. Failed/cancelled attempts
	// keep their logical workspace generation for resume/fresh retry; a
	// completed logical dispatch retires that path permanently. The operator
	// can still inspect failed output, matching finishRun's cancel/default
	// branches.
	if dispatchErr == nil {
		c.cleanupWorkspace(entry, hooks.BeforeRemove, env)
	}

	c.postFinished(entry.IssueID, dispatchErr)
}

func (c *Dispatcher) postFinished(issueID string, err error) {
	c.postCmd(cmdRunFinished{issueID: issueID, err: err})
}

// postCmd posts a command to the actor from a worker/discovery goroutine,
// abandoning the send if the dispatcher is shutting down (c.stop closed) so
// a late goroutine never leaks blocked on a full channel after the actor has
// exited. The single choke point for the actor-bound blocking send shared by
// postFinished, launchDiscovery, and the off-actor steps to come (ADR-028).
func (c *Dispatcher) postCmd(command cmd) {
	select {
	case c.cmds <- command:
	case <-c.stop:
	}
}

// revertTransition undoes the in-progress UpdateState performed at
// Claim time, moving the issue back to its original source state.
// Best-effort by design — failures are logged but don't propagate:
//
//   - sourceState == "" means we never transitioned in the first place,
//     so there's nothing to revert.
//   - The safety check (RefreshStates) compares the issue's current
//     tracker state against currentTarget (cfg.Agent.RunningState at
//     the time of dispatch). If the workflow has already moved the
//     issue forward (e.g. docs-refresh → "review") or the operator has
//     dragged it manually, we leave it alone — clobbering operator
//     actions would surprise the human in the loop.
//
// Used by dispatch's rollback paths (workspace-create-fail,
// runID-mint-fail), finishRun's cancel branch, and shutdown.
//
// The ctx passed in is used for the safety check + UpdateState; for
// detached call sites (finishRun, shutdown) callers should pass a
// short-budget context.Background()-derived ctx so an actor-shutdown
// doesn't short-circuit the revert.
func (c *Dispatcher) revertTransition(ctx context.Context, issueID, identifier, sourceState, currentTarget string) {
	if sourceState == "" {
		return
	}
	// Safety check: only revert if the issue is STILL in the target
	// running state. If the workflow already moved it (typical clean
	// finish path; the docs-refresh bot does this explicitly) or the
	// operator dragged it on the kanban, leave the new state alone.
	// RefreshStates is the cheapest read on the Tracker interface.
	if currentTarget != "" {
		if states, err := c.tracker.RefreshStates(ctx, []string{issueID}); err == nil {
			cur, present := states[issueID]
			switch {
			case !present:
				// Issue disappeared from the tracker — nothing to do.
				return
			case cur != currentTarget:
				c.logger.Debug("dispatcher: %s already moved %s → %s, skipping revert to %s", identifier, currentTarget, cur, sourceState)
				return
			}
		} else {
			// Couldn't verify — log and skip the revert. Leaving the
			// issue in the running state is safer than clobbering an
			// unknown state.
			c.logger.Warn("dispatcher: refresh state for revert %s: %v", identifier, err)
			return
		}
	}
	if err := c.tracker.UpdateState(ctx, issueID, sourceState); err != nil {
		if !errors.Is(err, tracker.ErrTransitionRejected) && !errors.Is(err, tracker.ErrNotSupported) {
			c.logger.Warn("dispatcher: revert state %s → %s: %v", identifier, sourceState, err)
		}
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
