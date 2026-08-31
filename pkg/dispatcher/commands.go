package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
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
	// Bounded: a wedged docker daemon must not pin this detached
	// goroutine (and its child process) forever — cmd.Run() below would
	// otherwise block indefinitely with no caller able to cancel it.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "rm", "--force", containerName)
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

// scheduleForceRemoveSandboxContainer fires forceRemoveSandboxContainer after
// cancelGracePeriod in a detached goroutine. The work is intentionally
// fire-and-forget — it must outlive the cancel command (and even a dispatcher
// shutdown) so a lingering container is still reaped — but it is wrapped in a
// recover so a panic in the docker-rm path can't crash the process, matching
// the actor loop's safeTick / safeCmdApply discipline.
func scheduleForceRemoveSandboxContainer(logger *iterlog.Logger, runID string) {
	go func() {
		defer func() {
			if r := recover(); r != nil && logger != nil {
				logger.Error("dispatcher: panic in deferred force-remove of run %s: %v", runID, r)
			}
		}()
		time.Sleep(cancelGracePeriod)
		forceRemoveSandboxContainer(logger, runID)
	}()
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

// cmdCandidates carries the result of an off-actor candidate discovery
// (launchDiscovery → tracker.ListCandidates) back to the actor. The actor
// runs the fast in-memory dispatch decision on the posted list — sort,
// dispatch-skip prune, per-issue cap checks + dispatch — keeping the
// blocking ListCandidates HTTP off the actor goroutine. See ADR-028 Step 2.
type cmdCandidates struct {
	issues []tracker.Issue
	err    error
}

func (m cmdCandidates) apply(c *Dispatcher, ctx context.Context) {
	// The discovery goroutine has returned; let the next tick start a fresh
	// one. Cleared first so an early return below (error / re-gate) doesn't
	// strand the flag and wedge discovery forever.
	c.state.discoveryInFlight = false

	if m.err != nil {
		c.logger.Warn("dispatcher: tracker ListCandidates: %v", m.err)
		c.state.lastTrackerErr = m.err.Error()
		c.state.lastTrackerErrAt = time.Now().UTC()
		c.fireSnapshot()
		return
	}
	// Clear the sticky tracker error once a poll succeeds so the
	// dashboard banner drops as soon as the operator fixes the token.
	c.state.lastTrackerErr = ""
	c.state.lastTrackerErrAt = time.Time{}

	// Discovery ran asynchronously, so the gates tick() checked before
	// launching it may have flipped in the meantime. Re-check the cheap
	// ones (pause + cost cap) so we don't dispatch into a state the
	// operator just closed. Concurrency is re-validated per-issue by the
	// dispatch loop's MaxConcurrent / hasSlot checks below.
	cfg := c.cfg.Load()
	if c.paused.Load() {
		c.fireSnapshot()
		return
	}
	if cc := c.state.costCap; cc != nil && cc.Exceeded {
		c.fireSnapshot()
		return
	}

	candidates := m.issues
	sortCandidates(candidates)

	// Prune dispatch-skip entries whose issue is no longer an eligible,
	// unclaimed candidate — it was closed or dragged out of the ready lane,
	// so the stale "won't dispatch" reason should disappear from the UI.
	// A journalled claim is the exception: deliberately parked cards are absent
	// from ListCandidates but their operator-facing reason must remain visible.
	// Issues that re-skip below re-populate the map; ones that became
	// dispatchable are cleared by dispatch() when they claim.
	if len(c.state.dispatchSkips) > 0 || len(c.state.lastRunHoldWarned) > 0 {
		live := make(map[string]struct{}, len(candidates))
		for _, iss := range candidates {
			live[iss.ID] = struct{}{}
		}
		if len(c.state.dispatchSkips) > 0 {
			for id := range c.state.dispatchSkips {
				if _, ok := live[id]; !ok && !c.claims.Contains(id) {
					delete(c.state.dispatchSkips, id)
				}
			}
		}
		if len(c.state.lastRunHoldWarned) > 0 {
			for id := range c.state.lastRunHoldWarned {
				if _, ok := live[id]; !ok {
					delete(c.state.lastRunHoldWarned, id)
				}
			}
		}
	}

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

// cmdDispatchSetupDone is posted by the off-actor dispatch-setup worker
// (runDispatchSetup, ADR-028 Step 4) once the post-claim in-progress
// UpdateState + workspaces.Create have run. It records the (possibly-performed)
// transition source onto the running entry and clears setupPending so the
// actor's reapers (refreshRunningStates, reconcileStalled) resume evaluating
// the entry. On setup FAILURE (err != nil) it reuses the Step-3 finishRun
// teardown — slot free + transition revert (reads the TransitionedFromState we
// set here) + claim release + retry — rather than duplicating it.
//
// All c.state writes for the claimed→running setup land here, on the actor:
// the worker only ever posts this value-copy command.
type cmdDispatchSetupDone struct {
	issueID          string
	transitionedFrom string
	err              error
}

func (m cmdDispatchSetupDone) apply(c *Dispatcher, ctx context.Context) {
	r, ok := c.state.running[m.issueID]
	if !ok {
		// The entry was already torn down (e.g. shutdown drained it) while
		// setup was in flight. The setupPending guards keep the reapers from
		// reaping it mid-setup, so this is rare; nothing to record. On the
		// failure path finishRun is a no-op for a missing entry anyway.
		return
	}
	// Record the transition source BEFORE any teardown so finishRun's revert
	// (and refreshRunningStates' expected-state comparison) read the move the
	// worker actually performed off-actor.
	r.TransitionedFromState = m.transitionedFrom
	r.setupPending = false
	if m.err != nil {
		// Setup failed (workspaces.Create). Reuse the Step-3 path.
		c.finishRun(ctx, m.issueID, m.err)
		return
	}
	c.fireSnapshot()
}

// cmdDropRetry drops a pending retry entry. Posted by the off-actor finish
// worker (runFinishWorker) when an exhausted run's give-up transition to
// FailedState SUCCEEDS: finishRun optimistically scheduled the retry on the
// actor as an in-memory re-dispatch guard for the worker's HTTP window, and
// once the issue has actually landed in the terminal state that guard must
// drop so the give-up is final (no retry). See ADR-028 Step 3.
type cmdDropRetry struct {
	issueID string
}

func (m cmdDropRetry) apply(c *Dispatcher, _ context.Context) {
	if cur, ok := c.state.retries[m.issueID]; ok {
		if cur.Timer != nil {
			cur.Timer.Stop()
		}
		delete(c.state.retries, m.issueID)
	}
	c.fireSnapshot()
}

// finishKind selects which tracker transition the off-actor finish worker
// performs in addition to the always-present Release. The actor decides the
// kind synchronously from c.state (in finishRun) and the worker only executes
// the chosen HTTP — it never reads c.state. See ADR-028 Step 3.
type finishKind int

const (
	// finishCompleted: clean finish — maybeTransitionToCompleted, then Release.
	finishCompleted finishKind = iota
	// finishRevert: cancellation or a non-exhausted failure — revertTransition
	// back to the source state (the retry, if any, was scheduled on the actor),
	// then Release.
	finishRevert
	// finishGiveUp: exhausted failure — UpdateState to FailedState; on success
	// post cmdDropRetry (drop the optimistically-scheduled guard) and Release;
	// on failure keep the retry (revertTransition + Release), matching today's
	// "board can't represent failed → preserve unbounded retry" fallback.
	finishGiveUp
)

// finishPlan is the value-copy of everything the off-actor finish worker
// (runFinishWorker) needs to run finishRun's tracker HTTP. It carries NO
// pointers into c.state or the *runningEntry — the actor computes it
// synchronously, captures the immutable inputs, and the worker reads only
// these fields plus dispatcher-immutables (c.tracker, c.logger, c.hostMarker).
// This is the anti-race boundary for ADR-028 Step 3: cfg-derived transition
// targets are captured here on the actor, never re-read by the worker.
type finishPlan struct {
	kind           finishKind
	issueID        string
	identifier     string
	runningTarget  string // cfg.Agent.RunningState snapshot at finish time
	completedState string // cfg.Agent.CompletedState snapshot (clean finish only)
	sourceState    string // r.TransitionedFromState (revert / give-up fallback)
	failedState    string // cfg.Agent.FailedState snapshot (give-up only)
	attemptCount   int    // r.Attempt+1, for give-up logging only
	runID          string // for give-up logging only
	runErrText     string // for give-up logging only
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

	// A run that suspended for input is NOT a failure. ErrRunPaused is a
	// human node awaiting an answer (status paused_waiting_human);
	// ErrRunPausedOperator is a soft pause the operator requested from the
	// run console (status paused_operator). Both leave a valid checkpoint
	// plus a pending interaction. The old code let these fall through to the
	// `default:` arm, which scheduled a retry that re-ran FRESH — paused
	// runs are not in resumableRunID's set — re-hit the same pause, and
	// eventually exhausted attempts into FailedState ("blocked"), bouncing
	// the ticket between states and burying the bot's escalation question.
	//
	// Instead, PARK it: keep the tracker claim (ListCandidates only returns
	// UNCLAIMED issues, so the retained claim is what stops the next tick
	// re-dispatching it — RunningState is itself an eligible candidate
	// state), free the concurrency slot (done above), and leave the issue in
	// place. The operator answers + resumes from the run console; the issue's
	// last_run pointer (stamped here) links straight to the paused run and
	// its pending interaction. This mirrors the cloud runner, which acks
	// ErrRunPaused / ErrRunPausedOperator instead of naking for retry
	// (pkg/runner/loop.go). The retained claim is reclaimed only by the
	// stale-claim sweep once THIS daemon's pid dies (isStaleLocalMarker), so
	// a live dispatcher never re-dispatches a parked issue. After a
	// restart the stale-claim sweep drops the claim; dispatch then
	// re-parks (awaiting-input, same last_run) instead of minting a
	// fresh run from entry — a planner restarted from init_film is
	// not "the same pause".
	if isResumeSourceChanged(err) {
		// The bot source changed since this run started. Minting a
		// sibling from entry would replay the prefix (agents, tools,
		// already-paid artefacts). Park: keep the claim, keep last_run,
		// do not retry. The operator resumes THIS run with --force; cancel
		// is not an escape because a cancelled last_run still forbids a
		// fresh sibling (the way out is --clear-last-run). Same operator
		// visibility as the pause arm below — badge, awaiting-input
		// column, and a dashboard skip entry — otherwise the ticket
		// strands invisibly: claimed, no live worker, no badge, and
		// absent from the snapshot's running and retry tables.
		c.stampLastRun(issueID, r)
		c.setAwaitingInput(issueID, true)
		c.moveToAwaitingInput(issueID, r.Identifier)
		c.recordDispatchSkip(tracker.Issue{ID: issueID, Identifier: r.Identifier},
			"bot source changed since the last run started — resume with --force; cancelling does not free the ticket (a cancelled last_run still holds it; use --clear-last-run)")
		c.logger.Warn("dispatcher: %s bot source changed (run=%s) — refusing a fresh sibling. Resume with --force from the run console; cancelling does not unblock a fresh dispatch (a cancelled last_run still holds the ticket; use --clear-last-run).", r.Identifier, r.RunID)
		c.fireSnapshot()
		return
	}

	if errors.Is(err, runtime.ErrRunPaused) || errors.Is(err, runtime.ErrRunPausedOperator) {
		c.stampLastRun(issueID, r)
		c.setAwaitingInput(issueID, true)
		// Move the card into the dedicated "awaiting input" column so the
		// board shows "this pipeline needs me" at the column level (not only
		// the per-card badge). Best-effort: a custom board without the state,
		// or an external tracker that rejects it, keeps the card in place (the
		// retained claim already blocks re-dispatch either way).
		c.moveToAwaitingInput(issueID, r.Identifier)
		c.logger.Info("dispatcher: %s paused awaiting input (run=%s): %v — moved to awaiting-input, kept claimed, NOT retried. Resume it from the run console; the issue keeps a live link via last_run.", r.Identifier, r.RunID, err)
		c.fireSnapshot()
		return
	}

	// One cfg snapshot for the whole teardown: the plan's running/completed/
	// failed-state targets, the honesty-guard log, and the give-up move all
	// read from the same config, so a mid-finish Reload can't split the
	// decision across two configs.
	cfg := c.cfg.Load()
	_ = ctx // caller ctx not used for the release; kept in signature for future audit hooks

	// Stamp the run + workdir back onto the tracker issue so the studio
	// can pivot from the kanban card to the run console / diff inspector.
	// Best-effort: only native trackers implement this, and a transient
	// disk failure shouldn't block the cleanup path. Stays on the actor:
	// SetLastRun is a native-tracker local write (a no-op for github/forgejo),
	// not the blocking tracker HTTP that ADR-028 Step 3 offloads.
	c.stampLastRun(issueID, r)

	// Decide the outcome on the actor — all c.state mutation (retry
	// scheduling, retries-clear) happens here, synchronously, and we capture
	// the immutable inputs the tracker HTTP needs into a value-copy plan. The
	// blocking tracker calls (Release + the chosen transition/revert) then run
	// off the actor in runFinishWorker. See ADR-028 Step 3. The worker always
	// does the transition FIRST and Release LAST: that keeps the tracker claim
	// held (so ListCandidates filters the issue) until it has been moved to its
	// final, mostly-non-eligible state — closing the re-dispatch window a
	// release-first ordering would open now that the HTTP no longer runs
	// atomically on the actor.
	plan := finishPlan{
		issueID:       issueID,
		identifier:    r.Identifier,
		runningTarget: cfg.Agent.RunningState,
		// Always captured; only the revert + give-up-fallback paths read it,
		// and it's harmlessly unused on the clean-finish path.
		sourceState: r.TransitionedFromState,
	}
	switch {
	case err == nil:
		// A clean terminal run is no longer awaiting input — clear the
		// denormalized board badge (best-effort HINT; see setAwaitingInput).
		c.setAwaitingInput(issueID, false)
		// Honesty guard: a "clean" finish with no commit produced nothing
		// directly mergeable, yet the transition below still moves the
		// issue to CompletedState (default "review") — where an operator
		// reasonably expects a diff to review. Surface the gap loudly
		// instead of letting an empty run masquerade as completed work.
		// Common causes: the wrong bot for the task (a feature ticket sent
		// to an improve-loop, which approves the unchanged code and exits
		// via its legit streak→done edge), or a run that left changes
		// uncommitted (needs commit-and-finalize). Non-fatal: we still
		// transition (reverting would loop the dispatcher), just visibly.
		if c.runFinalCommit(r.RunID) == "" {
			c.logger.Warn("dispatcher: %s finished cleanly but produced NO commit (run=%s) — nothing directly mergeable. Moving to %q anyway; inspect before merging (wrong bot for the task, or work left uncommitted → commit-and-finalize).", r.Identifier, r.RunID, cfg.Agent.CompletedState)
		} else {
			c.logger.Info("dispatcher: %s finished cleanly (run=%s)", r.Identifier, r.RunID)
		}
		// Successful dispatches clear any prior retry bookkeeping and
		// honor the workspace-persist policy. We don't revert the
		// in-progress transition — the workflow may have moved the
		// state itself (e.g. docs-refresh → "review"). When the workflow
		// did NOT move the state (most often because it lacks
		// board.move capability — dispatcher_default is the
		// archetypal case), an explicit move to CompletedState (done by
		// the worker via maybeTransitionToCompleted) prevents the next
		// tick from re-picking the same issue: RunningState is marked
		// eligible:true on the board (needed for crash-recovery), so
		// without this transition a no-board-move workflow would loop
		// indefinitely and burn model spend on every poll interval.
		if cur, ok := c.state.retries[issueID]; ok {
			if cur.Timer != nil {
				cur.Timer.Stop()
			}
			delete(c.state.retries, issueID)
		}
		plan.kind = finishCompleted
		plan.completedState = cfg.Agent.CompletedState
		// NOTE: workspace teardown (incl. the before_remove hook) is NOT
		// done here. It runs on the dispatch worker goroutine in runWorker,
		// before postFinished — so a shell hook can't block the actor and
		// the directory is gone before the claim is released. See
		// cleanupWorkspace + runWorker.
	case errors.Is(err, context.Canceled):
		// Cancellation is a soft stop. Keep the workspace and any
		// pending retry entry so the next tick can re-pick the issue.
		// Revert the in-progress transition so the next dispatch sees
		// the issue back in its source state (typically "ready"). The
		// safety check inside revertTransition skips when the workflow
		// or operator already moved the state elsewhere.
		c.logger.Info("dispatcher: %s cancelled (run=%s)", r.Identifier, r.RunID)
		plan.kind = finishRevert
	default:
		// Non-cancellation failure → retry, unless the attempt ceiling is
		// reached. On exhaustion, give up: move the issue to a terminal
		// FailedState (default "blocked") so the failure is visible on the
		// board and the issue stops being eligible — instead of rescheduling
		// forever and silently bouncing a doomed ticket between its source
		// and running states (burning model spend with no board signal).
		c.logger.Warn("dispatcher: %s failed (run=%s): %v", r.Identifier, r.RunID, err)
		// scheduleRetry runs on the actor in BOTH branches. In the exhausted
		// branch it is OPTIMISTIC: the retry entry is the in-memory
		// re-dispatch guard (isClaimed) that blocks a tick from re-picking
		// the issue while the worker's give-up HTTP is in flight. The worker
		// drops it (cmdDropRetry) iff the FailedState move succeeds; if the
		// move is unavailable the retry stays, exactly reproducing today's
		// giveUpIfExhausted fallback (board can't represent "failed" →
		// preserve unbounded retry rather than freeze the ticket).
		c.scheduleRetry(issueID, r, err)
		if exhausted(cfg, r) {
			plan.kind = finishGiveUp
			plan.failedState = cfg.Agent.FailedState
			plan.attemptCount = r.Attempt + 1
			plan.runID = r.RunID
			if err != nil {
				plan.runErrText = err.Error()
			}
		} else {
			plan.kind = finishRevert
		}
	}

	// Publish the freed-slot snapshot BEFORE handing the Release HTTP to the
	// off-actor worker. launchFinish spawns the worker, which enters
	// tracker.Release as soon as the runtime schedules it; if fireSnapshot ran
	// after launchFinish that scheduling could let the worker enter Release
	// (and, in tests, fire releaseEntered) while the published snapshot still
	// showed the slot in use (GlobalUsed=1). That window is a flake under CI's
	// scheduling and — per ADR-028 Step 3 — a real violation of "slot
	// accounting is observable before the release HTTP completes": the studio
	// could read the slot as used while Release is in flight. The paused
	// branch above already publishes before returning; the worker reads only
	// the immutable plan, so publishing first is safe.
	c.fireSnapshot()
	c.launchFinish(plan)
}

// exhausted reports whether a failing issue has reached the configured
// attempt ceiling (cfg.Agent.MaxAttempts; 0/negative = no cap) AND the board
// can represent a terminal FailedState. It is the pure in-memory gate of the
// former giveUpIfExhausted — the actor calls it synchronously to decide
// between the give-up and the normal retry path; the actual terminal move
// (tracker UpdateState) runs off the actor in runFinishWorker. Returns false
// when the cap is disabled, attempts remain, or FailedState is unset — in all
// of which the issue must keep retrying. (Whether the terminal MOVE itself
// then succeeds is decided by the worker: if the tracker rejects it, the
// optimistically-scheduled retry is preserved, reproducing the legacy
// "board can't represent failed → keep retrying" fallback.) Takes the caller's
// cfg snapshot so the give-up gate and the FailedState move it guards read the
// same config. Runs on the actor goroutine.
func exhausted(cfg *Config, r *runningEntry) bool {
	max := cfg.Agent.MaxAttempts
	// r.Attempt is 0-indexed (0 = initial run), so r.Attempt+1 is the
	// number of attempts made so far. Give up once that reaches the cap.
	if max <= 0 || r.Attempt+1 < max {
		return false
	}
	return cfg.Agent.FailedState != ""
}

// launchFinish runs a finishPlan's tracker HTTP (Release + the chosen
// transition/revert) on a short-lived goroutine OFF the actor and, for the
// give-up-success case, posts cmdDropRetry back. The actor has already done
// every c.state mutation (slot-free, retry/give-up bookkeeping) in finishRun;
// the worker reads ONLY the value-copy plan plus dispatcher-immutables
// (c.tracker, c.logger, c.hostMarker) — never c.state or c.cfg. Tracked on
// workersWG so Stop() drains it; the cmdDropRetry send is guarded by c.stop
// (via postCmd). Mirrors launchDiscovery (ADR-028 Step 2). See ADR-028 Step 3.
func (c *Dispatcher) launchFinish(plan finishPlan) {
	c.workersWG.Add(1)
	go func() {
		defer c.workersWG.Done()
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("dispatcher: panic in finish worker for %s: %v", plan.identifier, r)
			}
		}()
		c.runFinishWorker(plan)
	}()
}

// runFinishWorker executes a finishPlan off the actor goroutine. It performs
// the chosen transition FIRST and Release LAST (see finishRun's plan comment
// for the re-dispatch-window rationale), using a background-derived context so
// the release/transition survive a run-context or shutdown cancellation — a
// stuck "claimed" label on GitHub would otherwise block the next dispatcher
// from re-picking the issue. Best-effort throughout: errors are logged, never
// fatal, matching finishRun's prior on-actor behaviour.
func (c *Dispatcher) runFinishWorker(plan finishPlan) {
	if c.beforeFinishWorker != nil {
		c.beforeFinishWorker(plan)
	}

	relCtx, relCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer relCancel()

	switch plan.kind {
	case finishCompleted:
		c.maybeTransitionToCompleted(relCtx, plan.issueID, plan.identifier, plan.runningTarget, plan.completedState)
	case finishRevert:
		// Revert the in-progress transition so the next retry/dispatch tick
		// sees the issue eligible again from its source state. Without it the
		// issue would sit in `in_progress` (no longer eligible) until the
		// operator dragged it back.
		c.revertTransition(relCtx, plan.issueID, plan.identifier, plan.sourceState, plan.runningTarget)
	case finishGiveUp:
		if err := c.tracker.UpdateState(relCtx, plan.issueID, plan.failedState); err != nil {
			// The board can't represent "failed" (state undefined, transition
			// rejected, or a transient tracker error) — preserve the legacy
			// unbounded retry rather than freeze the ticket: revert to the
			// source state and KEEP the retry finishRun optimistically
			// scheduled (no cmdDropRetry).
			c.logger.Warn("dispatcher: %s exhausted %d attempts but the move to failed state %q failed (%v) — keeping retry behaviour", plan.identifier, plan.attemptCount, plan.failedState, err)
			c.revertTransition(relCtx, plan.issueID, plan.identifier, plan.sourceState, plan.runningTarget)
		} else {
			// Terminal move succeeded — the give-up is final, so drop the
			// optimistic retry guard the actor scheduled.
			c.logger.Warn("dispatcher: %s gave up after %d attempts (run=%s): %s — moved to %q; clear the blocker or re-open the issue to retry", plan.identifier, plan.attemptCount, plan.runID, plan.runErrText, plan.failedState)
			// Stamp the ticket so downstream readers can tell this move
			// apart from an operator filing the same state by hand — the
			// pipeline board needs it to surface the failure instead of
			// filing it as acknowledged history.
			c.stampGiveUp(plan)
			c.postCmd(cmdDropRetry{issueID: plan.issueID})
		}
	}

	c.releaseClaim(relCtx, plan.issueID, plan.identifier)
}

// releaseClaim releases the tracker claim, tolerating the benign races where
// the claim is already gone (ErrNotFound) or held by someone else
// (ErrClaimConflict). Best-effort: any other error is logged at warn. Shared
// by the finish worker; the issue is re-pickable by the next tick once this
// returns (subject to the issue's final tracker state and any retry guard).
func (c *Dispatcher) releaseClaim(ctx context.Context, issueID, identifier string) {
	if err := c.tracker.Release(ctx, issueID, c.hostMarker); err != nil &&
		!errors.Is(err, tracker.ErrNotFound) &&
		!errors.Is(err, tracker.ErrClaimConflict) {
		c.logger.Warn("dispatcher: release %s: %v", identifier, err)
	}
	// Drop the journal entry even on the benign races above — in all of
	// them the claim is no longer ours to recover at the next boot.
	c.claims.Remove(issueID)
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

// setAwaitingInput denormalizes the pause-hint flag onto the tracker
// issue so the board grid can badge the card without an N+1 run fetch.
// Only native trackers implement SetAwaitingInput — external trackers
// (github, forgejo) silently skip via the failed type-assertion, same
// as stampLastRun. Best-effort: a write failure is logged at warn and
// does not derail the pause/finish/dispatch path.
//
// Set true when a run parks on pause; cleared false on the paths the
// dispatcher controls (clean terminal finish, re-dispatch). It is a
// HINT — a console-only resume the dispatcher never observes leaves the
// flag stale until the next card touch corrects it; the modal's
// authoritative check stays getRun(last_run_id).status.
func (c *Dispatcher) setAwaitingInput(issueID string, v bool) {
	setter, ok := c.tracker.(interface {
		SetAwaitingInput(id string, v bool) error
	})
	if !ok {
		return
	}
	if err := setter.SetAwaitingInput(issueID, v); err != nil {
		c.logger.Warn("dispatcher: set awaiting-input=%v on %s: %v", v, issueID, err)
	}
}

// giveUpStamper is the optional seam a board-backed tracker implements to
// record a give-up. Named rather than inlined at the assertion so a test can
// hold the shipped adapters to it: an assertion on an anonymous interface
// fails SILENTLY, which is how the SetLastRun pass-through went missing and no
// issue got its pointer for a release.
type giveUpStamper interface {
	SetGaveUp(id string, g *native.GiveUp) error
}

// stampGiveUp records on the tracker issue that THIS dispatcher filed it
// after exhausting its retry budget — the fact a bare terminal state cannot
// carry. Without it the pipeline board reads the give-up's own FailedState
// move as "a human already filed this ticket" and buries the failure in
// Closed, which is exactly the failure its Needs-attention lane exists for
// (issue #494).
//
// Same optional-interface seam as setAwaitingInput/stampLastRun: only board
// trackers implement it, external ones skip silently. Best-effort — a write
// failure is logged at warn and never derails the give-up, whose authority is
// the FailedState move that already landed.
//
// Called ONLY once that move succeeded: a give-up that fell back to retrying
// (the board cannot represent the failed state) has not given up.
func (c *Dispatcher) stampGiveUp(plan finishPlan) {
	setter, ok := c.tracker.(giveUpStamper)
	if !ok {
		return
	}
	stamp := &native.GiveUp{
		RunID:    plan.runID,
		State:    plan.failedState,
		Attempts: plan.attemptCount,
		At:       time.Now().UTC(),
	}
	if err := setter.SetGaveUp(plan.issueID, stamp); err != nil {
		c.logger.Warn("dispatcher: stamp give-up on %s: %v", plan.identifier, err)
	}
}

// moveToAwaitingInput best-effort transitions a paused card into the
// StateAwaitingInput column. A board without that state (custom schema) or an
// external tracker that rejects the transition leaves the card in place — the
// retained claim already blocks re-dispatch, so this is a display-only
// refinement, never load-bearing.
func (c *Dispatcher) moveToAwaitingInput(issueID, identifier string) bool {
	if err := c.tracker.UpdateState(context.Background(), issueID, native.StateAwaitingInput); err != nil {
		c.logger.Info("dispatcher: %s stays in place (no awaiting-input column): %v", identifier, err)
		return false
	}
	return true
}

// maybeTransitionToCompleted moves a cleanly-finished issue from
// RunningState into CompletedState when the workflow itself didn't
// move it elsewhere. No-op when:
//   - CompletedState is empty (operator opted out via `none`).
//   - CompletedState equals RunningState (the transition would be
//     a no-op anyway, and trackers may reject same-state moves).
//   - The current tracker state differs from RunningState — that's
//     the "workflow already moved it" case (docs-refresh → review,
//     a board-aware bot picking "done", etc.); leave it alone.
//   - Tracker rejects the transition (state not defined on the
//     board, blocking guard, etc.) — log + leave in RunningState.
//     Operators with custom boards aren't forced to opt out.
//
// The motivation lives in the cfg.Agent.CompletedState comment in
// config.go; this helper is just the application path. Detached
// ctx is the caller's relCtx so a winding-down dispatcher still
// completes the move (parallels the Release path).
func (c *Dispatcher) maybeTransitionToCompleted(ctx context.Context, issueID, identifier, runningTarget, completed string) {
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

// probeRunJSON reads <storeDir>/runs/<runID>/run.json and decodes it into
// dst (a pointer to a struct selecting the fields of interest by json tag).
// Returns false when storeDir/runID is unset, the file is missing, or the
// JSON can't be decoded — every call site treats a false/zero result as
// "fall back to the safe default". On failure dst is left untouched, so a
// caller can ignore the bool and read dst's zero value.
func (c *Dispatcher) probeRunJSON(runID string, dst any) bool {
	if c.storeDir == "" || runID == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(c.storeDir, "runs", runID, "run.json"))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, dst) == nil
}

// resolveRunWorkdir reads the run's persisted WorkDir from
// <storeDir>/runs/<runID>/run.json. Returns "" when storeDir is
// unset, the file is missing, or the JSON can't be decoded — every
// call site treats "" as "fall back to the dispatcher workspace".
func (c *Dispatcher) resolveRunWorkdir(runID string) string {
	var probe struct {
		WorkDir string `json:"work_dir"`
	}
	c.probeRunJSON(runID, &probe)
	return probe.WorkDir
}

// runFinalCommit reads the run's persisted FinalCommit from
// <storeDir>/runs/<runID>/run.json. Returns "" when storeDir is unset,
// the file is missing, the JSON can't be decoded, or the run produced
// no commit (worktree HEAD unchanged, or work left uncommitted). Callers
// treat "" as "this run produced nothing directly mergeable".
func (c *Dispatcher) runFinalCommit(runID string) string {
	var probe struct {
		FinalCommit string `json:"final_commit"`
	}
	c.probeRunJSON(runID, &probe)
	return probe.FinalCommit
}

// cleanupWorkspace tears down one generation after a clean dispatch when the
// active persist policy calls for it. The optional before_remove hook runs
// while the checkout still exists, so custom configurations can archive output
// or perform their own teardown.
//
// When the workspace is a linked Git worktree, its common Git directory is
// captured before the hook runs. After the owned directory is gone we remove
// that exact registration. Deregistering after deletion is important: Git can
// never race a final commit in a live checkout, and unrelated stale
// registrations remain untouched. Discovering the common directory from the
// checkout also covers custom after_create hooks, rather than coupling cleanup
// to BuildDefaultConfig's project path.
//
// MUST be called from the dispatch worker goroutine (runWorker), never the
// actor: the hook is a shell command bounded only by its own timeout
// (default 60s) and would otherwise stall polling/dispatch/snapshots.
// beforeRemove is the hook snapshotted by the caller (nil = no-op; Hook.Run
// tolerates a nil receiver); env is the same ITERION_* set the other hooks
// receive. Retirement is fail-closed: if ownership cannot be revoked, neither
// the hook nor deletion runs. New cleanup-policy dispatches use unique paths,
// so partial teardown cannot strand them. A failing hook is logged but the
// retired directory is still removed.
func (c *Dispatcher) cleanupWorkspace(entry *runningEntry, beforeRemove *Hook, env []string) {
	if entry == nil || !entry.CleanupWorkspaceOnSuccess {
		return
	}
	// Stranded-work guard: the external workspace is a detached-HEAD
	// worktree with NO finalize/commit stage of its own. A bot without
	// `worktree: auto` writes straight into it — if it exits without
	// committing, directory removal would silently destroy finished work,
	// unrecoverable even from the reflog. Keep the workspace instead and
	// say so; the operator inspects/commits by hand. Probe failures
	// (non-git workspace, git missing) fall through to normal cleanup —
	// the guard only bites on positive evidence.
	if dirty, err := workspaceIsDirty(entry.WorkspacePath); err == nil && dirty {
		c.logger.Warn("dispatcher: workspace %s has UNCOMMITTED changes — keeping %s (inspect and commit by hand; cleanup skipped)",
			entry.Identifier, entry.WorkspacePath)
		return
	}
	workspaceGeneration := entry.WorkspaceGeneration
	if err := c.workspaces.RetireForRun(entry.IssueID, workspaceGeneration); err != nil {
		c.logger.Warn("dispatcher: preserving workspace %s at %s — cannot retire ownership: %v",
			entry.Identifier, entry.WorkspacePath, err)
		return
	}
	// Best-effort discovery. A plain directory or standalone repository has
	// no surviving metadata to update after its directory disappears; a
	// linked worktree resolves to the host repository's common gitdir.
	gitCommonDir, _ := workspaceGitCommonDir(entry.WorkspacePath)
	if err := beforeRemove.Run(context.Background(), c.logger, "before_remove", entry.WorkspacePath, env); err != nil {
		c.logger.Warn("dispatcher: before_remove hook for %s: %v", entry.Identifier, err)
	}
	if err := c.workspaces.RemoveForRun(entry.IssueID, workspaceGeneration); err != nil {
		c.logger.Warn("dispatcher: cleanup workspace %s: %v", entry.Identifier, err)
	}
	// A custom before_remove hook may itself remove the checkout. Exact
	// deregistration is still safe whenever the path is now absent. If a late
	// writer recreated it, retain the registration fail-closed.
	if gitCommonDir != "" {
		if _, err := os.Lstat(entry.WorkspacePath); errors.Is(err, os.ErrNotExist) {
			if err := removeGitWorktreeRegistration(gitCommonDir, entry.WorkspacePath); err != nil {
				c.logger.Warn("dispatcher: remove git worktree registration for %s: %v", entry.Identifier, err)
			}
		}
	}
}

// workspaceIsDirty reports whether the workspace's git working tree has
// uncommitted changes (`git status --porcelain` non-empty). An error means
// the probe itself failed (not a git checkout, git absent) — callers treat
// that as "unknown", not as dirty.
func workspaceIsDirty(path string) (bool, error) {
	// Bounded: cleanup runs on a worker, but an unbounded git process would
	// still prevent the claim from being released and the issue from
	// becoming dispatchable again.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = path
	// cmd.Dir names the repository; an inherited GIT_DIR or GIT_INDEX_FILE
	// would answer about another one, or read an index that is not its own.
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// workspaceGitCommonDir resolves the repository metadata directory shared by
// all worktrees while path is still present. Only a common directory outside
// path is returned: a standalone repository's .git directory disappears with
// the workspace and must never be used as a post-delete cleanup target.
func workspaceGitCommonDir(path string) (string, error) {
	workspaceDir, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("canonicalize git workspace: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Do not use rev-parse --path-format=absolute here: that flag requires Git
	// 2.31, while --git-common-dir itself works on every Git version that
	// supports linked worktrees. Resolve its possibly-relative output below.
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = workspaceDir
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	commonDir := strings.TrimSpace(string(out))
	if commonDir == "" {
		return "", errors.New("git returned an empty common directory")
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(workspaceDir, commonDir)
	}
	commonDir, err = filepath.EvalSymlinks(filepath.Clean(commonDir))
	if err != nil {
		return "", fmt.Errorf("canonicalize git common directory: %w", err)
	}
	if isWithin(commonDir, workspaceDir) {
		return "", errors.New("git common directory is inside workspace")
	}
	return commonDir, nil
}

// removeGitWorktreeRegistration removes one exact, already-deleted checkout
// registration. Listing first makes this idempotent with custom before_remove
// hooks that already deregistered the checkout.
func removeGitWorktreeRegistration(commonDir, workspacePath string) error {
	out, err := listGitWorktrees(commonDir, true)
	nulDelimited := err == nil
	if err != nil {
		// `worktree list -z` was added after --porcelain. Generated workspace
		// names contain no newlines, but a configured root technically can; in
		// that exceptional case the legacy line format cannot be parsed
		// unambiguously and we fail closed with an upgrade hint.
		if strings.ContainsAny(workspacePath, "\r\n") {
			return fmt.Errorf("git worktree list -z unsupported for a newline-containing workspace path (Git 2.36+ required): %w", err)
		}
		legacyOut, legacyErr := listGitWorktrees(commonDir, false)
		if legacyErr != nil {
			return fmt.Errorf("git worktree list: %w", errors.Join(err, legacyErr))
		}
		out = legacyOut
	}
	found := false
	separator := []byte{0}
	if !nulDelimited {
		separator = []byte{'\n'}
	}
	for _, field := range bytes.Split(out, separator) {
		const prefix = "worktree "
		if !bytes.HasPrefix(field, []byte(prefix)) {
			continue
		}
		registeredPath := filepath.Clean(string(bytes.TrimPrefix(field, []byte(prefix))))
		if registeredPath == filepath.Clean(workspacePath) {
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	removeCtx, cancelRemove := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelRemove()
	cmd := exec.CommandContext(removeCtx, "git", "--git-dir", commonDir, "worktree", "remove", "--force", workspacePath)
	// --git-dir overrides GIT_DIR but NOT GIT_COMMON_DIR, and the worktree
	// registry is exactly one of the non-worktree files git takes from there.
	// Inherited, this --force removal would deregister a worktree in ANOTHER
	// repository while ours keeps a stale registration.
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func listGitWorktrees(commonDir string, nulDelimited bool) ([]byte, error) {
	listCtx, cancelList := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelList()
	args := []string{"--git-dir", commonDir, "worktree", "list", "--porcelain"}
	if nulDelimited {
		args = append(args, "-z")
	}
	listCmd := exec.CommandContext(listCtx, "git", args...)
	// Same reason as the removal above: --git-dir does not neutralise
	// GIT_COMMON_DIR, so this would list another repository's worktrees.
	listCmd.Env = gitlib.SanitizeEnv(os.Environ())
	return listCmd.Output()
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
	// Operator cancel: nil cause → the engine persists terminal `cancelled`,
	// which the dispatcher never auto-resumes (resume is an explicit gesture).
	if r.Cancel != nil {
		r.Cancel(nil)
	}
	c.logger.Info("dispatcher: %s cancel requested", r.Identifier)
	// Belt-and-braces: after a short grace period for the engine's
	// defer chain to clean up, force-remove any lingering sandbox
	// container. Without this, `claude --print` inside the container
	// often outlives the host-side docker exec SIGKILL and keeps
	// streaming events for tens of seconds — operators perceive cancel
	// as broken.
	scheduleForceRemoveSandboxContainer(c.logger, r.RunID)
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
			r.Cancel(nil) // operator cancel — terminal, never auto-resumed
		}
		c.logger.Info("dispatcher: %s cancel requested (run %s)", r.Identifier, m.runID)
		scheduleForceRemoveSandboxContainer(c.logger, r.RunID)
		m.reply <- true
		return
	}
	m.reply <- false
}
