package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// branchCancelGracePeriod bounds how long the fan-out collector waits,
// after cancellation, for still-running branches to honour ctx and
// return. Branches that observe ctx (the production backends kill their
// subprocess / abort the stream) return well within this; the bound only
// matters for a branch wedged in executor.Execute that ignores ctx —
// without it the collector would block forever on that branch's result.
// A package var so tests can shorten it; operators tune it via
// ITERION_BRANCH_CANCEL_GRACE (a Go duration, e.g. "30s") when their
// backends need longer to unwind on cancellation.
var branchCancelGracePeriod = defaultBranchCancelGracePeriod()

func defaultBranchCancelGracePeriod() time.Duration {
	if v := os.Getenv("ITERION_BRANCH_CANCEL_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Second
}

// ---------------------------------------------------------------------------
// Fan-out / Join — parallel branch scheduler
// ---------------------------------------------------------------------------

// execFanOut handles a fan_out_all router node by spawning parallel
// branches for each outgoing edge, bounded by MaxParallelBranches.
// It returns the next node ID to continue from (after the join).
func (e *Engine) execFanOut(ctx context.Context, rs *runState, routerNodeID string) (string, error) {
	if err := e.emitRouterPassThrough(rs, routerNodeID); err != nil {
		return "", err
	}

	plan, err := e.prepareFanOut(rs, routerNodeID)
	if err != nil {
		return "", err
	}
	starts := make(map[string]string, len(plan.edges))
	for _, edge := range plan.edges {
		starts[fmt.Sprintf("branch_%s_%s", routerNodeID, edge.To)] = edge.To
	}
	parallel, err := e.ensureParallelInvocation(rs, routerNodeID, starts)
	if err != nil {
		return "", err
	}

	// Derive a cancellable context for the whole fan-out. When any branch
	// trips the budget (or the parent ctx is cancelled — Ctrl-C), cancelling
	// branchCtx stops siblings racking up tokens/USD on subsequent LLM calls
	// (a fan_out_all with N branches and a $10 cap would otherwise burn
	// N * $10 worst-case before stopping). defer-cancel guards leaks.
	branchCtx, cancelBranches := context.WithCancel(ctx)
	defer cancelBranches()

	resultsCh := e.launchBranches(branchCtx, cancelBranches, rs, routerNodeID, plan, parallel)
	results, ctxErr := e.collectBranches(ctx, branchCtx, cancelBranches, resultsCh, plan.branchIDs(routerNodeID), rs, routerNodeID, "fan_out")
	parallel.retire()
	if ctxErr != nil {
		return "", e.wrapContextErr(ctxErr)
	}

	if err := pausedBranchError(results); err != nil {
		return "", err
	}
	next, err := e.resolveConvergence(rs, routerNodeID, results, plan)
	if err == nil {
		rs.parallel = nil
	}
	return next, err
}

// fanOutPlan is the resolved launch plan for a fan_out_all router, computed
// once by prepareFanOut and consumed by launchBranches / resolveConvergence.
type fanOutPlan struct {
	edges                  []*ir.Edge
	maxParallel            int
	preComputedConvergence string
	cancelOnFirstFailure   bool
	parentOutputs          map[string]map[string]any
	parentArtifacts        map[string]map[string]any
}

// emitRouterPassThrough emits the fan_out router's node_started /
// node_finished pair and records its pass-through output (router output =
// its input from incoming edges).
func (e *Engine) emitRouterPassThrough(rs *runState, routerNodeID string) error {
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, routerNodeID, map[string]any{
		"kind":      "router",
		"mode":      "fan_out_all",
		"iteration": e.currentLoopIteration(routerNodeID, rs.loopCounters),
	}); err != nil {
		return err
	}
	routerInput := e.buildNodeInputRS(routerNodeID, rs.scope())
	rs.outputs[routerNodeID] = routerInput
	return e.emit(rs.ctx, rs.runID, store.EventNodeFinished, routerNodeID, nil)
}

// prepareFanOut resolves everything launchBranches needs before spawning:
// the router's outgoing edges (workspace-safety checked), the concurrency
// cap, the pre-computed convergence point, the sibling-cancellation policy,
// and deep copies of parent outputs/artifacts so branches can't mutate
// shared state.
//
// cancelOnFirstFailure: under wait_all (the default convergence strategy)
// any branch failure dooms the whole run, so the first error cancels
// siblings to stop them spending tokens/USD on work that will be discarded.
// Under best_effort, sibling failures are tolerated — the convergence
// aggregator still consumes successful branches — so peer failures must NOT
// cancel siblings (only budget exhaustion / parent ctx, which apply globally).
func (e *Engine) prepareFanOut(rs *runState, routerNodeID string) (fanOutPlan, error) {
	var fanEdges []*ir.Edge
	for _, edge := range e.workflow.Edges {
		if edge.From == routerNodeID {
			fanEdges = append(fanEdges, edge)
		}
	}
	if len(fanEdges) == 0 {
		return fanOutPlan{}, fmt.Errorf("fan_out_all router %q has no outgoing edges", routerNodeID)
	}
	return e.planFromEdges(rs, routerNodeID, fanEdges)
}

// planFromEdges builds the launch plan for a set of already-resolved
// fan-out edges: workspace-safety check, concurrency cap, pre-computed
// convergence point, sibling-cancellation policy, and deep copies of
// parent outputs/artifacts. Shared by prepareFanOut (fan_out_all router)
// and execLLMRouterMulti (llm router multi-select).
func (e *Engine) planFromEdges(rs *runState, routerNodeID string, fanEdges []*ir.Edge) (fanOutPlan, error) {
	if err := e.validateWorkspaceSafety(routerNodeID, fanEdges); err != nil {
		return fanOutPlan{}, err
	}

	maxParallel := len(fanEdges)
	if e.workflow.Budget != nil && e.workflow.Budget.MaxParallelBranches > 0 && e.workflow.Budget.MaxParallelBranches < maxParallel {
		maxParallel = e.workflow.Budget.MaxParallelBranches
	}

	preComputedConvergence := e.findConvergencePoint(routerNodeID, fanEdges)
	cancelOnFirstFailure := true
	if preComputedConvergence != "" {
		if convNode, ok := e.workflow.Nodes[preComputedConvergence]; ok {
			if mode := nodeAwaitMode(convNode); mode == ir.AwaitBestEffort {
				cancelOnFirstFailure = false
			}
		}
	}

	return fanOutPlan{
		edges:                  fanEdges,
		maxParallel:            maxParallel,
		preComputedConvergence: preComputedConvergence,
		cancelOnFirstFailure:   cancelOnFirstFailure,
		parentOutputs:          copyOutputs(rs.outputs),
		parentArtifacts:        copyOutputs(rs.artifacts),
	}, nil
}

// branchIDs lists the branch identifiers this plan will launch, in edge
// order — the same "branch_<router>_<target>" ids launchBranches assigns.
// The collector diffs them against the returned results to name abandoned
// branches.
func (p fanOutPlan) branchIDs(routerNodeID string) []string {
	ids := make([]string, 0, len(p.edges))
	for _, edge := range p.edges {
		ids = append(ids, fmt.Sprintf("branch_%s_%s", routerNodeID, edge.To))
	}
	return ids
}

// branchSlot is the fan-out semaphore slot held by one branch goroutine. A
// parked `wait` node releases its slot (so the emitting branch can acquire one
// and fire the event) and reacquires it on wake — without this, a `wait` would
// hold its slot for the whole park and an under-provisioned
// max_parallel_branches would starve the emitter until the wait timed out
// (ADR-051 "releasing the slot on park" follow-on). release/acquire are
// idempotent on `held` so the launchBranches defer always releases exactly the
// final hold, never double-counting. The slot is owned by a single branch
// goroutine, so `held` needs no synchronization.
type branchSlot struct {
	sem  chan struct{}
	held bool
}

// release frees the slot if currently held (no-op otherwise).
func (s *branchSlot) release() {
	if s != nil && s.held {
		<-s.sem
		s.held = false
	}
}

// acquire takes a slot if not already held, blocking until one is free or ctx
// is cancelled. Returns ctx.Err() if cancelled before a slot is obtained.
func (s *branchSlot) acquire(ctx context.Context) error {
	if s == nil || s.held {
		return nil
	}
	select {
	case s.sem <- struct{}{}:
		s.held = true
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// launchBranches spawns one bounded goroutine per fan-out edge and returns
// the buffered results channel (sized to len(plan.edges), so a wedged
// branch's eventual send never blocks the collector). Each goroutine
// registers panic recovery FIRST, then acquires a semaphore slot — bailing
// with a cancelled result if branchCtx is already done (so a queued branch
// doesn't block on a slot held by a doomed sibling). After execBranch it
// cancels siblings when the branch tripped the budget (always) or failed
// under wait_all (cancelOnFirstFailure).
func (e *Engine) launchBranches(branchCtx context.Context, cancelBranches context.CancelFunc, rs *runState, routerNodeID string, plan fanOutPlan, parallel *parallelExecutionState) <-chan *branchResult {
	sem := make(chan struct{}, plan.maxParallel)
	resultsCh := make(chan *branchResult, len(plan.edges))

	for _, edge := range plan.edges {
		branchID := fmt.Sprintf("branch_%s_%s", routerNodeID, edge.To)

		go func(edge *ir.Edge, branchID string) {
			// Panic-recovery defer FIRST, before the semaphore acquire —
			// otherwise a panic before the recover() defer is registered
			// would be unrecoverable.
			defer func() {
				if r := recover(); r != nil {
					parallel.releaseResumeWaiters(branchID)
					cancelBranches()
					resultsCh <- &branchResult{
						branchID: branchID,
						outputs:  make(map[string]map[string]any),
						err:      fmt.Errorf("panic in branch %s: %v", branchID, r),
					}
				}
			}()
			if err := parallel.waitResumeTurn(branchCtx, branchID); err != nil {
				resultsCh <- &branchResult{branchID: branchID, outputs: make(map[string]map[string]any), err: e.wrapContextErr(err)}
				return
			}
			// Acquire a slot, but bail if the fan-out is already cancelled
			// (budget trip, sibling failure with wait_all, or parent cancel)
			// — otherwise a branch queued behind maxParallel would block on a
			// slot held by a branch wedged in executor.Execute even though its
			// result is already doomed. The cancelled result keeps the
			// collector's count balanced (no branch_started was emitted yet).
			// The slot is a *branchSlot so a parked wait can release+reacquire
			// it mid-branch; the defer releases whatever the branch still holds.
			slot := &branchSlot{sem: sem}
			if err := slot.acquire(branchCtx); err != nil {
				resultsCh <- &branchResult{
					branchID: branchID,
					outputs:  make(map[string]map[string]any),
					err:      e.wrapContextErr(branchCtx.Err()),
				}
				return
			}
			defer slot.release()

			result := e.execBranch(branchCtx, rs, branchID, edge, plan.parentOutputs, plan.parentArtifacts, plan.preComputedConvergence, slot, parallel)
			// Cancel siblings (they observe it via the ctx.Done() select at
			// the top of their per-iteration loop) when this branch tripped
			// the global budget — every fan_out regardless of await mode — or
			// failed for any reason under wait_all (their results would be
			// discarded anyway, so paying for them is pure waste).
			if result != nil && result.err != nil {
				if errors.Is(result.err, ErrRunPaused) || errors.Is(result.err, ErrBudgetExceeded) || plan.cancelOnFirstFailure {
					cancelBranches()
				}
			}
			// Whatever the exit, siblings parked on this branch's resume
			// barrier must not outlive it (a no-op unless it was the
			// answered branch and it never reached its successor cursor).
			parallel.releaseResumeWaiters(branchID)
			resultsCh <- result
		}(edge, branchID)
	}
	return resultsCh
}

func pausedBranchError(results []*branchResult) error {
	for _, result := range results {
		if result != nil && errors.Is(result.err, ErrRunPaused) {
			return ErrRunPaused
		}
	}
	return nil
}

// collectBranches drains one result per expected branch. It observes both the
// parent ctx and the fan-out's internal branchCtx. Parent cancellation is
// surfaced to the caller as ctx.Err(); internal cancellation (sibling failure,
// budget trip) only starts the bounded grace drain so the original branch
// result is preserved. After cancellation it bounds the wait by
// branchCancelGracePeriod, then abandons any still-running branches (their
// buffered sends never block) — naming each one and persisting a
// branch_abandoned event so the leak is visible in the run record, not just
// a process log line.
func (e *Engine) collectBranches(ctx context.Context, branchCtx context.Context, cancelBranches context.CancelFunc, resultsCh <-chan *branchResult, expected []string, rs *runState, routerNodeID, mode string) ([]*branchResult, error) {
	total := len(expected)
	results := make([]*branchResult, 0, total)
	var ctxErr error
	parentDoneCh := ctx.Done()
	branchDoneCh := branchCtx.Done()
	var graceCh <-chan time.Time
	var graceTimer *time.Timer
	startGrace := func() {
		if graceTimer != nil {
			return
		}
		graceTimer = time.NewTimer(branchCancelGracePeriod)
		graceCh = graceTimer.C
	}
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()
	for collected := 0; collected < total; {
		select {
		case r := <-resultsCh:
			results = append(results, r)
			collected++
		case <-parentDoneCh:
			ctxErr = ctx.Err()
			cancelBranches()
			parentDoneCh = nil
			branchDoneCh = nil
			startGrace()
		case <-branchDoneCh:
			if err := ctx.Err(); err != nil {
				ctxErr = err
				cancelBranches()
				parentDoneCh = nil
			}
			branchDoneCh = nil
			startGrace()
		case <-graceCh:
			if abandoned := total - collected; abandoned > 0 {
				e.recordAbandonedBranches(rs, routerNodeID, mode, expected, results)
			}
			collected = total
		}
	}
	return results, ctxErr
}

// recordAbandonedBranches names the branches that never delivered a result
// and persists one branch_abandoned event per branch. The emit uses a
// non-cancellable ctx: abandonment happens precisely when the run/fan-out
// context is already cancelled, and a ctx-sensitive store (Mongo) would
// otherwise drop the very event that documents the leak.
func (e *Engine) recordAbandonedBranches(rs *runState, routerNodeID, mode string, expected []string, results []*branchResult) {
	returned := make(map[string]bool, len(results))
	for _, r := range results {
		if r != nil {
			returned[r.branchID] = true
		}
	}
	emitCtx := context.WithoutCancel(rs.ctx)
	for _, branchID := range expected {
		if returned[branchID] {
			continue
		}
		if e.logger != nil {
			e.logger.Warn("%s from %s: abandoning branch %s still running %s after cancellation (wedged in executor.Execute?)", mode, routerNodeID, branchID, branchCancelGracePeriod)
		}
		if err := e.emit(emitCtx, rs.runID, store.EventBranchAbandoned, branchID, map[string]any{
			"router":       routerNodeID,
			"mode":         mode,
			"grace_period": branchCancelGracePeriod.String(),
			"reason":       "cancelled; still running after the grace period",
		}); err != nil && e.logger != nil {
			e.logger.Warn("%s from %s: emit branch_abandoned for %s: %v", mode, routerNodeID, branchID, err)
		}
	}
}

// resolveConvergence picks the node the fan-out continues from and processes
// it. It prefers the convergence point reported by successful branches; if
// all branches failed it falls back to the pre-computed topology point.
//
// Under best_effort, branches may legitimately end at different nodes (one
// fails, one is cancelled, one completes) — keep the first non-empty
// joinNodeID and log the divergence rather than aborting (which would discard
// the successful branches, exactly the failure mode best_effort exists to
// avoid). When no branch reported a convergence point and every branch ran
// cleanly to its own Done node (all-done topology under best_effort), hand a
// terminal node back so the engine routes to run_finished.
func (e *Engine) resolveConvergence(rs *runState, routerNodeID string, results []*branchResult, plan fanOutPlan) (string, error) {
	convergenceNodeID := ""
	isBestEffort := !plan.cancelOnFirstFailure
	for _, r := range results {
		if r.joinNodeID == "" {
			continue
		}
		if convergenceNodeID == "" {
			convergenceNodeID = r.joinNodeID
			continue
		}
		if convergenceNodeID != r.joinNodeID {
			if isBestEffort {
				if e.logger != nil {
					e.logger.Warn("fan_out from %s: branches converge to different nodes (%s vs %s in branch %s) — best_effort, keeping first",
						routerNodeID, convergenceNodeID, r.joinNodeID, r.branchID)
				}
				continue
			}
			return "", fmt.Errorf("branches converge to different nodes: %s vs %s", convergenceNodeID, r.joinNodeID)
		}
	}
	if convergenceNodeID == "" {
		if allTerminatedAtDone(results) && (isBestEffort || plan.preComputedConvergence == "") {
			strategy := ir.AwaitWaitAll
			if isBestEffort {
				strategy = ir.AwaitBestEffort
			}
			return e.processConvergenceTerminal(rs, results, strategy)
		}
		convergenceNodeID = plan.preComputedConvergence
		if convergenceNodeID == "" {
			return "", fmt.Errorf("no convergence point found after fan_out from %s", routerNodeID)
		}
	}

	return e.processConvergence(rs, convergenceNodeID, results)
}

// allTerminatedAtDone reports whether every branch finished cleanly at
// an *ir.DoneNode. Branches with err != nil count as non-terminating —
// a terminal convergence requires every branch to have produced a clean
// terminal exit, regardless of whether sibling failure policy was wait_all
// or best_effort.
func allTerminatedAtDone(results []*branchResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r.err != nil || !r.terminatedAtDone || r.terminalNodeID == "" {
			return false
		}
	}
	return true
}
