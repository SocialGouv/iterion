package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// branchCancelGracePeriod bounds how long the fan-out collector waits,
// after cancellation, for still-running branches to honour ctx and
// return. Branches that observe ctx (the production backends kill their
// subprocess / abort the stream) return well within this; the bound only
// matters for a branch wedged in executor.Execute that ignores ctx —
// without it the collector would block forever on that branch's result.
// A package var so tests can shorten it.
var branchCancelGracePeriod = 5 * time.Second

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

	// Derive a cancellable context for the whole fan-out. When any branch
	// trips the budget (or the parent ctx is cancelled — Ctrl-C), cancelling
	// branchCtx stops siblings racking up tokens/USD on subsequent LLM calls
	// (a fan_out_all with N branches and a $10 cap would otherwise burn
	// N * $10 worst-case before stopping). defer-cancel guards leaks.
	branchCtx, cancelBranches := context.WithCancel(ctx)
	defer cancelBranches()

	resultsCh := e.launchBranches(branchCtx, cancelBranches, rs, routerNodeID, plan)
	results, ctxErr := e.collectBranches(ctx, cancelBranches, resultsCh, len(plan.edges), routerNodeID)
	if ctxErr != nil {
		return "", e.wrapContextErr(ctxErr)
	}

	return e.resolveConvergence(rs, routerNodeID, results, plan)
}

// fanOutPlan is the resolved launch plan for a fan_out_all router, computed
// once by prepareFanOut and consumed by launchBranches / resolveConvergence.
type fanOutPlan struct {
	edges                  []*ir.Edge
	maxParallel            int
	preComputedConvergence string
	cancelOnFirstFailure   bool
	parentOutputs          map[string]map[string]interface{}
	parentArtifacts        map[string]map[string]interface{}
}

// emitRouterPassThrough emits the fan_out router's node_started /
// node_finished pair and records its pass-through output (router output =
// its input from incoming edges).
func (e *Engine) emitRouterPassThrough(rs *runState, routerNodeID string) error {
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, routerNodeID, map[string]interface{}{
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

// launchBranches spawns one bounded goroutine per fan-out edge and returns
// the buffered results channel (sized to len(plan.edges), so a wedged
// branch's eventual send never blocks the collector). Each goroutine
// registers panic recovery FIRST, then acquires a semaphore slot — bailing
// with a cancelled result if branchCtx is already done (so a queued branch
// doesn't block on a slot held by a doomed sibling). After execBranch it
// cancels siblings when the branch tripped the budget (always) or failed
// under wait_all (cancelOnFirstFailure).
func (e *Engine) launchBranches(branchCtx context.Context, cancelBranches context.CancelFunc, rs *runState, routerNodeID string, plan fanOutPlan) <-chan *branchResult {
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
					resultsCh <- &branchResult{
						branchID: branchID,
						outputs:  make(map[string]map[string]interface{}),
						err:      fmt.Errorf("panic in branch %s: %v", branchID, r),
					}
				}
			}()
			// Acquire a slot, but bail if the fan-out is already cancelled
			// (budget trip, sibling failure with wait_all, or parent cancel)
			// — otherwise a branch queued behind maxParallel would block on a
			// slot held by a branch wedged in executor.Execute even though its
			// result is already doomed. The cancelled result keeps the
			// collector's count balanced (no branch_started was emitted yet).
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }() // release
			case <-branchCtx.Done():
				resultsCh <- &branchResult{
					branchID: branchID,
					outputs:  make(map[string]map[string]interface{}),
					err:      e.wrapContextErr(branchCtx.Err()),
				}
				return
			}

			result := e.execBranch(branchCtx, rs, branchID, edge, plan.parentOutputs, plan.parentArtifacts, plan.preComputedConvergence)
			// Cancel siblings (they observe it via the ctx.Done() select at
			// the top of their per-iteration loop) when this branch tripped
			// the global budget — every fan_out regardless of await mode — or
			// failed for any reason under wait_all (their results would be
			// discarded anyway, so paying for them is pure waste).
			if result != nil && result.err != nil {
				if errors.Is(result.err, ErrBudgetExceeded) || plan.cancelOnFirstFailure {
					cancelBranches()
				}
			}
			resultsCh <- result
		}(edge, branchID)
	}
	return resultsCh
}

// collectBranches drains `total` branch results. It is ctx-aware: if the
// parent ctx fires (run cancellation, timeout) it cancels branches and keeps
// draining — branches that already started but ignore ctx (e.g. a
// claude_code subprocess that swallows SIGINT) must not leak goroutines — but
// records the cancellation so the aggregate error reflects it. After
// cancellation it bounds the wait by branchCancelGracePeriod, then abandons
// any still-running branches (their buffered sends never block). doneCh is
// niled after the first fire so the closed channel doesn't busy-spin.
func (e *Engine) collectBranches(ctx context.Context, cancelBranches context.CancelFunc, resultsCh <-chan *branchResult, total int, routerNodeID string) ([]*branchResult, error) {
	results := make([]*branchResult, 0, total)
	var ctxErr error
	doneCh := ctx.Done()
	var graceCh <-chan time.Time
	var graceTimer *time.Timer
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
		case <-doneCh:
			ctxErr = ctx.Err()
			cancelBranches()
			doneCh = nil
			graceTimer = time.NewTimer(branchCancelGracePeriod)
			graceCh = graceTimer.C
		case <-graceCh:
			if abandoned := total - collected; abandoned > 0 && e.logger != nil {
				e.logger.Warn("fan_out from %s: abandoning %d branch(es) still running %s after cancellation (wedged in executor.Execute?)", routerNodeID, abandoned, branchCancelGracePeriod)
			}
			collected = total
		}
	}
	return results, ctxErr
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
		if isBestEffort && allTerminatedAtDone(results) {
			return e.processConvergenceTerminal(rs, results)
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
// best_effort tolerates them but the all-done shortcut requires every
// branch to have produced a terminal exit.
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

// ---------------------------------------------------------------------------
// Branch execution
// ---------------------------------------------------------------------------

// branchResult holds the outcome of a single parallel branch.
type branchResult struct {
	branchID         string
	outputs          map[string]map[string]interface{}
	artifacts        map[string]map[string]interface{} // publish name → output
	artifactVersions map[string]int
	joinNodeID       string // the join node this branch converged to (empty if terminal)
	err              error
	eventErrors      int // count of event emission failures (best-effort events)
	// terminatedAtDone is true when the branch loop exited at an
	// *ir.DoneNode rather than at a convergence/error/cancel. Used by
	// best_effort fan_out to recognise the "every branch finished at
	// its own done" topology — without this flag the post-loop
	// convergence search would fail because joinNodeID is empty on each
	// branch and preComputedConvergence is also empty (the branches
	// diverge to distinct terminals).
	terminatedAtDone bool
	terminalNodeID   string // the *ir.DoneNode ID when terminatedAtDone
}

// execBranch runs a single parallel branch starting from the target of
// the given edge. It executes nodes sequentially until it reaches a
// convergence point, a terminal node, or encounters an error.
// convergenceNodeID is the pre-computed convergence point (may be empty
// if unknown; in that case, AwaitMode on individual nodes is checked).
func (e *Engine) execBranch(ctx context.Context, rs *runState, branchID string, startEdge *ir.Edge, parentOutputs map[string]map[string]interface{}, parentArtifacts map[string]map[string]interface{}, convergenceNodeID string) *branchResult {
	result := initBranchResult(rs, branchID)
	runID := rs.runID

	// branchCostUSD is this branch's cumulative LLM spend, recorded into the
	// shared daily-cap ledger under a per-branch key ("<runID>#<branchID>")
	// so concurrent branches don't clobber each other's monotonic-max entry
	// (see recordBranchUsage).
	var branchCostUSD float64

	// Emit branch_started (best-effort — branch can proceed without the event).
	if err := e.emitBranch(ctx, runID, branchID, store.EventBranchStarted, startEdge.To, nil); err != nil {
		e.logger.Warn("branch %s: failed to emit branch_started: %v", branchID, err)
		result.eventErrors++
	}

	// Always emit branch_finished, regardless of how the branch exits — the
	// started/finished pair tracks in-flight concurrency for observers.
	defer e.emitBranchFinishedDefer(ctx, runID, branchID, startEdge.To, result)

	currentNodeID := startEdge.To

	for {
		select {
		case <-ctx.Done():
			result.err = e.wrapContextErr(ctx.Err())
			return result
		default:
		}

		node, ok := e.workflow.Nodes[currentNodeID]
		if !ok {
			result.err = fmt.Errorf("node %q not found in branch %s", currentNodeID, branchID)
			return result
		}

		// Stop at convergence point — the branch has reached the
		// pre-computed node where parallel branches reconverge.
		if convergenceNodeID != "" && currentNodeID == convergenceNodeID {
			result.joinNodeID = currentNodeID
			return result
		}

		// Stop at terminal nodes within a branch.
		switch node.(type) {
		case *ir.DoneNode:
			result.terminatedAtDone = true
			result.terminalNodeID = currentNodeID
			return result
		case *ir.FailNode:
			result.err = fmt.Errorf("branch %s reached fail node %q", branchID, currentNodeID)
			return result
		}

		// Check budget before execution.
		if e.checkPreExecBudget(ctx, rs, runID, branchID, currentNodeID, result) {
			return result
		}

		// Loop edges inside fan-out branches are skipped (see helpers.go),
		// so iteration here reflects the parent loop counters only.
		iter := e.currentLoopIteration(currentNodeID, rs.loopCounters)

		// Emit node_started.
		if err := e.emitBranch(ctx, runID, branchID, store.EventNodeStarted, currentNodeID, map[string]interface{}{
			"kind":      node.NodeKind().String(),
			"iteration": iter,
		}); err != nil {
			e.logger.Warn("branch %s: failed to emit node_started: %v", branchID, err)
			result.eventErrors++
		}

		output, done := e.executeNodeForBranch(ctx, rs, runID, branchID, currentNodeID, node, parentOutputs, parentArtifacts, iter, result)
		if done {
			return result
		}

		if e.recordBranchUsage(ctx, rs, runID, branchID, currentNodeID, output, &branchCostUSD, result) {
			return result
		}

		if e.publishBranchArtifact(ctx, runID, branchID, currentNodeID, node, output, result) {
			return result
		}

		// Emit node_finished with usage data.
		if err := e.emitBranch(ctx, runID, branchID, store.EventNodeFinished, currentNodeID, buildNodeFinishedData(e.sanitizeOutputForEvent(node, output))); err != nil {
			e.logger.Warn("branch %s: failed to emit node_finished: %v", branchID, err)
			result.eventErrors++
		}
		if e.onNodeFinished != nil {
			e.onNodeFinished(runID, currentNodeID, output)
		}

		// Select next edge (branch-local, no loop counters needed in branches).
		nextNodeID, err := e.selectEdgeBranch(ctx, runID, branchID, currentNodeID, output)
		if err != nil {
			result.err = err
			return result
		}

		currentNodeID = nextNodeID
	}
}

// initBranchResult allocates a branch's result accumulator, copying the
// parent's artifact versions so the branch keeps incrementing from the
// correct version instead of resetting to 0 each fan-out cycle.
func initBranchResult(rs *runState, branchID string) *branchResult {
	branchArtifactVersions := make(map[string]int, len(rs.artifactVersions))
	for k, v := range rs.artifactVersions {
		branchArtifactVersions[k] = v
	}
	return &branchResult{
		branchID:         branchID,
		outputs:          make(map[string]map[string]interface{}),
		artifacts:        make(map[string]map[string]interface{}),
		artifactVersions: branchArtifactVersions,
	}
}

// emitBranchFinishedDefer emits branch_finished (with the branch's terminal
// error / join node, if any). Always invoked via defer so the
// started/finished pair closes on every exit path — observers (e.g. the
// Prometheus parallel-branches gauge) rely on it to track in-flight
// concurrency. result is taken by pointer so the deferred read sees the
// branch's final state.
func (e *Engine) emitBranchFinishedDefer(ctx context.Context, runID, branchID, startNodeID string, result *branchResult) {
	data := map[string]interface{}{}
	if result.err != nil {
		data["error"] = result.err.Error()
	}
	if result.joinNodeID != "" {
		data["join_node"] = result.joinNodeID
	}
	if err := e.emitBranch(ctx, runID, branchID, store.EventBranchFinished, startNodeID, data); err != nil {
		e.logger.Warn("branch %s: failed to emit branch_finished: %v", branchID, err)
		result.eventErrors++
	}
}

// checkPreExecBudget emits budget_exceeded and sets result.err (returning
// true → caller returns the branch) when the run budget is soft-exceeded or
// hard-limited before executing a node. Both checks emit the event before
// failing; emission failures are best-effort (counted on result.eventErrors).
func (e *Engine) checkPreExecBudget(ctx context.Context, rs *runState, runID, branchID, currentNodeID string, result *branchResult) bool {
	if rs.budget == nil {
		return false
	}
	checks := rs.budget.Check()
	if exc := findExceeded(checks); exc != nil {
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]interface{}{
			"dimension": exc.dimension,
			"used":      exc.used,
			"limit":     exc.limit,
		}); err != nil {
			e.logger.Warn("branch %s: failed to emit budget_exceeded: %v", branchID, err)
			result.eventErrors++
		}
		result.err = fmt.Errorf("%w: %s (%.0f/%.0f)", ErrBudgetExceeded, exc.dimension, exc.used, exc.limit)
		return true
	}
	if hl := findHardLimited(checks); hl != nil {
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]interface{}{
			"dimension":  hl.dimension,
			"used":       hl.used,
			"limit":      hl.limit,
			"hard_limit": true,
		}); err != nil {
			e.logger.Warn("branch %s: failed to emit budget hard limit: %v", branchID, err)
			result.eventErrors++
		}
		result.err = fmt.Errorf("%w: hard limit %s at %.0f%% (%.0f/%.0f)", ErrBudgetExceeded, hl.dimension, (hl.used/hl.limit)*100, hl.used, hl.limit)
		return true
	}
	return false
}

// executeNodeForBranch builds the node's input (parent outputs merged with
// branch-local outputs so upstream refs still resolve; the parent rs feeds
// {{loop.*}} / {{run.*}} read-only), runs the executor, records the output,
// and validates it against the declared schema. Returns the output and a done
// flag: done=true (with result.err set) when execution or validation failed.
// On an execution error it emits node_finished with the error so the event
// log stays paired.
func (e *Engine) executeNodeForBranch(ctx context.Context, rs *runState, runID, branchID, currentNodeID string, node ir.Node, parentOutputs, parentArtifacts map[string]map[string]interface{}, iter int, result *branchResult) (map[string]interface{}, bool) {
	merged := mergeOutputs(parentOutputs, result.outputs)
	mergedArt := mergeOutputs(parentArtifacts, result.artifacts)

	// Event-driven primitives (ADR-051) are engine-special, not executor-backed:
	// they touch the run-scoped event registry directly. A wait parked here
	// holds this branch's semaphore slot until the emitting branch fires the
	// event (or the mandatory timeout), so max_parallel_branches must admit
	// both branches concurrently. emit/wait carry no `needs:` and never shell
	// out, so they bypass the resource-lease + executor path below.
	switch n := node.(type) {
	case *ir.EmitNode:
		output := e.emitEvent(rs, n)
		result.outputs[currentNodeID] = output
		return output, false
	case *ir.WaitNode:
		output, werr := e.awaitEvent(ctx, rs, currentNodeID, n)
		if werr != nil {
			result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, werr)
			return nil, true
		}
		result.outputs[currentNodeID] = output
		if err := e.validateNodeOutput(currentNodeID, n, output); err != nil {
			result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, err)
			return nil, true
		}
		return output, false
	}

	nodeInput := e.buildNodeInputRS(currentNodeID, resolveScope{
		vars:      rs.vars,
		outputs:   merged,
		runInputs: rs.runInputs,
		artifacts: mergedArt,
		rs:        rs,
	})

	// Acquire this node's declared resources (`needs:`) before running. This
	// is the spot that bounds resource-heavy work INSIDE a fan-out: N branches
	// whose agent `needs: godot` are capped at the resource's capacity even
	// when max_parallel_branches is higher or unset. Released on return
	// (defer) so a failed branch node still frees its slot.
	releaseResources, leases, aerr := e.acquireResources(ctx, rs, ir.NodeNeeds(node))
	if aerr != nil {
		result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, aerr)
		return nil, true
	}
	defer releaseResources()
	if len(leases) > 0 {
		nodeInput[leaseInputKey] = leases // surface leased instance ids to the branch node
	}

	execCtx := model.WithLoopIteration(ctx, iter)
	output, err := e.executor.Execute(execCtx, node, nodeInput)
	if err != nil {
		result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, err)
		if emitErr := e.emitBranch(ctx, runID, branchID, store.EventNodeFinished, currentNodeID, map[string]interface{}{
			"error": err.Error(),
		}); emitErr != nil {
			e.logger.Warn("branch %s: failed to emit node_finished: %v", branchID, emitErr)
			result.eventErrors++
		}
		return nil, true
	}

	result.outputs[currentNodeID] = output

	if err := e.validateNodeOutput(currentNodeID, node, output); err != nil {
		result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, err)
		return nil, true
	}
	return output, false
}

// recordBranchUsage records a node's token/cost usage against the per-branch
// daily-cap ledger and the run budget, emits budget warnings, and fails the
// branch (returning true with result.err set) when a budget dimension is
// exceeded. The daily-cap key is "<runID>#<branchID>" so concurrent branches
// accumulate independently; the per-run budget pause decision stays on the
// trunk's pre-exec path, branches only contribute spend.
func (e *Engine) recordBranchUsage(ctx context.Context, rs *runState, runID, branchID, currentNodeID string, output map[string]interface{}, branchCostUSD *float64, result *branchResult) bool {
	tokens, costUSD := extractUsage(output)

	if e.dailyCap != nil && costUSD > 0 {
		*branchCostUSD += costUSD
		if _, err := e.dailyCap.Record(ctx, runID+"#"+branchID, *branchCostUSD); err != nil {
			e.logger.Warn("branch %s: daily spend cap record failed: %v", branchID, err)
		}
	}

	if rs.budget == nil {
		return false
	}
	checks := rs.budget.RecordUsage(tokens, costUSD)

	for _, w := range findWarnings(checks) {
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetWarning, currentNodeID, map[string]interface{}{
			"dimension": w.dimension,
			"used":      w.used,
			"limit":     w.limit,
		}); err != nil {
			e.logger.Warn("branch %s: failed to emit budget_warning: %v", branchID, err)
			result.eventErrors++
		}
	}

	if exc := findExceeded(checks); exc != nil {
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]interface{}{
			"dimension": exc.dimension,
			"used":      exc.used,
			"limit":     exc.limit,
		}); err != nil {
			e.logger.Warn("branch %s: failed to emit budget_exceeded: %v", branchID, err)
			result.eventErrors++
		}
		result.err = fmt.Errorf("%w: %s (%.0f/%.0f)", ErrBudgetExceeded, exc.dimension, exc.used, exc.limit)
		return true
	}
	return false
}

// publishBranchArtifact persists the node's output as a versioned artifact
// when the node declares publish, bumps the in-memory version, registers it
// under the publish name, and emits artifact_written. A write-store failure
// aborts the branch (returns true); an event-emit failure is best-effort. The
// persisted artifact records the OLD version while the in-memory map advances
// to the next.
func (e *Engine) publishBranchArtifact(ctx context.Context, runID, branchID, currentNodeID string, node ir.Node, output map[string]interface{}, result *branchResult) bool {
	pub := nodePublish(node)
	if pub == "" {
		return false
	}
	version := result.artifactVersions[currentNodeID]
	artifact := &store.Artifact{
		RunID:   runID,
		NodeID:  currentNodeID,
		Version: version,
		Data:    output,
	}
	if err := e.store.WriteArtifact(ctx, artifact); err != nil {
		result.err = fmt.Errorf("node %q in branch %s: write artifact: %w", currentNodeID, branchID, err)
		return true
	}
	result.artifactVersions[currentNodeID] = version + 1
	result.artifacts[pub] = output
	if err := e.emitBranch(ctx, runID, branchID, store.EventArtifactWritten, currentNodeID, map[string]interface{}{
		"publish": pub,
		"version": version,
	}); err != nil {
		e.logger.Warn("branch %s: failed to emit artifact_written: %v", branchID, err)
		result.eventErrors++
	}
	return false
}

// selectEdgeBranch picks the next node for a branch. It is simpler than
// selectEdge: no loop counter enforcement, events carry a branch ID.
func (e *Engine) selectEdgeBranch(ctx context.Context, runID, branchID, fromNodeID string, output map[string]interface{}) (string, error) {
	selected := e.evaluateEdges(fromNodeID, fmt.Sprintf("branch %s", branchID), output)
	if selected == nil {
		return "", fmt.Errorf("no outgoing edge from node %q in branch %s", fromNodeID, branchID)
	}

	if err := e.emitBranch(ctx, runID, branchID, store.EventEdgeSelected, "", map[string]interface{}{
		"from": selected.From,
		"to":   selected.To,
	}); err != nil {
		e.logger.Warn("branch %s: failed to emit edge_selected: %v", branchID, err)
	}

	return selected.To, nil
}

// ---------------------------------------------------------------------------
// Convergence / Join
// ---------------------------------------------------------------------------

// processConvergence aggregates branch results according to the convergence
// node's await strategy, merges outputs into the run state, builds the
// convergence node's input from multi-edge with-mappings, and returns
// the convergence node ID for the main loop to continue execution.
func (e *Engine) processConvergence(rs *runState, convergenceNodeID string, results []*branchResult) (string, error) {
	convNode, ok := e.workflow.Nodes[convergenceNodeID]
	if !ok {
		return "", fmt.Errorf("convergence node %q not found", convergenceNodeID)
	}

	// Determine await strategy: use node's explicit setting, default to wait_all.
	strategy := nodeAwaitMode(convNode)
	if strategy == ir.AwaitNone {
		strategy = ir.AwaitWaitAll
	}

	// Collect failed branches metadata.
	var failedBranches []map[string]interface{}
	for _, r := range results {
		if r.err != nil {
			failedBranches = append(failedBranches, map[string]interface{}{
				"branch_id": r.branchID,
				"error":     r.err.Error(),
			})
		}
	}

	// Apply await strategy.
	switch strategy {
	case ir.AwaitWaitAll:
		if len(failedBranches) > 0 {
			return "", fmt.Errorf("convergence at %s (wait_all): %d branch(es) failed: %v",
				convergenceNodeID, len(failedBranches), failedBranches[0]["error"])
		}
	case ir.AwaitBestEffort:
		// Proceed even with failures — failed branch metadata is exposed.
	}

	// Merge successful branch outputs into the run state.
	for _, r := range results {
		if r.err != nil {
			continue
		}
		for nodeID, output := range r.outputs {
			rs.outputs[nodeID] = output
		}
		for name, output := range r.artifacts {
			// Last-write-wins, but make a silent clobber observable: two
			// parallel branches publishing the same artifact name would
			// otherwise overwrite each other with no trace.
			if prev, ok := rs.artifacts[name]; ok && !reflect.DeepEqual(prev, output) {
				e.logger.Warn("convergence at %s: artifact %q published by multiple branches with differing values — last write wins",
					convergenceNodeID, name)
			}
			rs.artifacts[name] = output
		}
		for nodeID, version := range r.artifactVersions {
			rs.artifactVersions[nodeID] = version
		}
	}

	// Add failed branches metadata to outputs so it's available via with-mappings.
	if len(failedBranches) > 0 {
		// Expose as a special output on the convergence node.
		if rs.outputs[convergenceNodeID] == nil {
			rs.outputs[convergenceNodeID] = make(map[string]interface{})
		}
		rs.outputs[convergenceNodeID]["_failed_branches"] = failedBranches
	}

	// Emit convergence_ready event.
	convData := map[string]interface{}{
		"strategy": strategy.String(),
	}
	if len(failedBranches) > 0 {
		convData["failed_branches"] = failedBranches
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventJoinReady, convergenceNodeID, convData); err != nil {
		e.logger.Warn("failed to emit convergence_ready: %v", err)
	}

	// Return the convergence node ID — the main loop will execute it normally.
	return convergenceNodeID, nil
}

// processConvergenceTerminal handles the best_effort all-done topology
// (every branch ran to its own *ir.DoneNode and no branch failed).
// Merges branch outputs/artifacts into the run state and hands back one
// of the terminal node IDs so the engine's main loop emits run_finished.
func (e *Engine) processConvergenceTerminal(rs *runState, results []*branchResult) (string, error) {
	for _, r := range results {
		for nodeID, output := range r.outputs {
			rs.outputs[nodeID] = output
		}
		for name, output := range r.artifacts {
			rs.artifacts[name] = output
		}
		for nodeID, version := range r.artifactVersions {
			rs.artifactVersions[nodeID] = version
		}
	}
	// Use the first branch's terminal node — the engine treats any Done
	// node as run_finished, so picking one is unambiguous.
	terminal := results[0].terminalNodeID
	if err := e.emit(rs.ctx, rs.runID, store.EventJoinReady, terminal, map[string]interface{}{
		"strategy":       ir.AwaitBestEffort.String(),
		"terminal_join":  true,
		"branches_total": len(results),
	}); err != nil {
		e.logger.Warn("failed to emit terminal convergence join_ready: %v", err)
	}
	return terminal, nil
}

// findConvergencePoint walks outgoing edges from the router's targets to
// find a downstream convergence point (a node with AwaitMode != AwaitNone,
// or a node that receives edges from multiple distinct sources).
// Terminal nodes (done/fail) can be convergence points when multiple
// branches target them directly.
// This is also called pre-emptively before branches start so that each
// branch knows where to stop.
func (e *Engine) findConvergencePoint(routerNodeID string, fanEdges []*ir.Edge) string {
	// Build in-degree map: count distinct sources per target.
	inSources := make(map[string]map[string]bool)
	for _, edge := range e.workflow.Edges {
		if _, ok := inSources[edge.To]; !ok {
			inSources[edge.To] = make(map[string]bool)
		}
		inSources[edge.To][edge.From] = true
	}

	// BFS from each fan-out target to find a convergence point.
	// maxVisits guards against a malformed graph where a cycle slipped
	// past compile-time validation (C012/C013): without it the queue
	// could grow without bound. Cap at the workflow's node count —
	// any honest BFS visits each node at most once.
	maxVisits := len(e.workflow.Nodes) + 1
	for _, startEdge := range fanEdges {
		visited := map[string]bool{}
		queue := []string{startEdge.To}
		for len(queue) > 0 {
			if len(visited) > maxVisits {
				if e.logger != nil {
					e.logger.Warn("findConvergencePoint: BFS exceeded %d visits — likely an undetected graph cycle, aborting search", maxVisits)
				}
				break
			}
			nodeID := queue[0]
			queue = queue[1:]
			if visited[nodeID] {
				continue
			}
			visited[nodeID] = true

			node, ok := e.workflow.Nodes[nodeID]
			if !ok {
				continue
			}
			// Convergence point: explicitly marked OR has multiple distinct incoming sources.
			if nodeAwaitMode(node) != ir.AwaitNone || len(inSources[nodeID]) > 1 {
				return nodeID
			}
			// Follow outgoing edges.
			for _, edge := range e.workflow.Edges {
				if edge.From == nodeID {
					queue = append(queue, edge.To)
				}
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Output copy helpers
// ---------------------------------------------------------------------------

// mergeOutputs creates a merged view of parent and branch outputs.
// Branch outputs take precedence over parent outputs.
func mergeOutputs(parent, branch map[string]map[string]interface{}) map[string]map[string]interface{} {
	merged := make(map[string]map[string]interface{}, len(parent)+len(branch))
	for k, v := range parent {
		merged[k] = v
	}
	for k, v := range branch {
		merged[k] = v
	}
	return merged
}

// copyOutputs creates a deep copy of the outputs map so that concurrent
// branches cannot mutate shared parent state. Naive two-level copying
// (the previous implementation) left nested maps and slices aliased
// between branches: a fan-out where two branches both received an
// upstream output containing a nested map would race on that map's
// internal hashtable.
func copyOutputs(src map[string]map[string]interface{}) map[string]map[string]interface{} {
	dst := make(map[string]map[string]interface{}, len(src))
	for k, v := range src {
		inner := make(map[string]interface{}, len(v))
		for ik, iv := range v {
			inner[ik] = deepCopyValue(iv)
		}
		dst[k] = inner
	}
	return dst
}

// deepCopyValue recursively copies a value tree of the shapes produced
// by JSON unmarshalling (map[string]interface{}, []interface{}, plus
// scalars). Other concrete types pass through unchanged — the runtime
// only stores JSON-shaped values in node outputs, so this covers the
// real cases without paying the cost of reflection-based cloning.
func deepCopyValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			out[k] = deepCopyValue(val)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = deepCopyValue(val)
		}
		return out
	default:
		return v
	}
}

// ---------------------------------------------------------------------------
// Workspace mutation safety
// ---------------------------------------------------------------------------

// readOnlyTools is the set of built-in tool names that are guaranteed to
// never modify the workspace. These are safe for parallel execution.
var readOnlyTools = map[string]bool{
	"git_diff":        true,
	"git_status":      true,
	"read_file":       true,
	"list_files":      true,
	"search_codebase": true,
	"tree":            true,
}

// isMutatingNode returns true if the node may modify the workspace.
// Tool nodes are always mutating. Agent/judge nodes are mutating only
// if they have at least one tool that is not in the read-only set.
// Nodes with Readonly=true are never considered mutating.
func isMutatingNode(node ir.Node) bool {
	switch n := node.(type) {
	case *ir.ToolNode:
		return true
	case *ir.AgentNode:
		if n.Readonly {
			return false
		}
		for _, t := range n.Tools {
			if !readOnlyTools[t] {
				return true
			}
		}
	case *ir.JudgeNode:
		if n.Readonly {
			return false
		}
		for _, t := range n.Tools {
			if !readOnlyTools[t] {
				return true
			}
		}
	}
	return false
}

// branchContainsMutation walks from startNodeID to globalConvergence (or to a
// terminal node) and returns true if any node along the path may mutate the
// workspace.
//
// The previous implementation stopped walking at the FIRST node with
// AwaitMode != AwaitNone — i.e. at any intermediate join — which meant that
// in a topology like
//
//	router(fan_out_all) -> A -> joinA -> mutA -> globalJoin
//	                    -> B -> joinB -> mutB -> globalJoin
//
// the BFS treated `joinA` / `joinB` as the stopping point and never saw
// `mutA` or `mutB`. Both branches passed validateWorkspaceSafety, then ran
// in parallel and raced on the shared workspace (e.g. git index).
//
// The correct stopping condition is the GLOBAL convergence point of the
// fan-out (the node where all branches reconverge), not the first
// intermediate join. We pass that in explicitly. Terminal nodes (done/fail)
// also stop the walk because the branch ends there.
func (e *Engine) branchContainsMutation(startNodeID, globalConvergence string) bool {
	visited := map[string]bool{}
	queue := []string{startNodeID}
	for len(queue) > 0 {
		nodeID := queue[0]
		queue = queue[1:]
		if visited[nodeID] {
			continue
		}
		visited[nodeID] = true

		// Stop at the global convergence point — beyond it, nodes are
		// post-fan-out and shared by all branches sequentially.
		if globalConvergence != "" && nodeID == globalConvergence {
			continue
		}

		node, ok := e.workflow.Nodes[nodeID]
		if !ok {
			continue
		}
		// Stop walking at terminal nodes.
		if isTerminalNode(node) {
			continue
		}
		if isMutatingNode(node) {
			return true
		}
		for _, edge := range e.workflow.Edges {
			if edge.From == nodeID {
				queue = append(queue, edge.To)
			}
		}
	}
	return false
}

// validateWorkspaceSafety checks that at most one branch in a fan-out
// contains mutating nodes. Returns an error if the topology is unsafe.
//
// routerNodeID + fanEdges are used to compute the global convergence point
// up-front; we pass it down to branchContainsMutation so the BFS doesn't
// stop early at intermediate joins.
func (e *Engine) validateWorkspaceSafety(routerNodeID string, fanEdges []*ir.Edge) error {
	globalConvergence := e.findConvergencePoint(routerNodeID, fanEdges)
	mutatingCount := 0
	var mutatingBranches []string
	for _, edge := range fanEdges {
		if e.branchContainsMutation(edge.To, globalConvergence) {
			mutatingCount++
			mutatingBranches = append(mutatingBranches, edge.To)
		}
	}
	if mutatingCount > 1 {
		return &RuntimeError{
			Code:    ErrCodeWorkspaceSafety,
			Message: fmt.Sprintf("workspace safety violation: %d branches contain mutating nodes %v", mutatingCount, mutatingBranches),
			Hint:    "at most 1 mutating branch is allowed in parallel on the same workspace; move tool nodes to separate sequential steps",
		}
	}
	return nil
}
