package runtime

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Branch execution
// ---------------------------------------------------------------------------

// branchResult holds the outcome of a single parallel branch.
type branchResult struct {
	branchID         string
	outputs          map[string]map[string]any
	artifacts        map[string]map[string]any // publish name → output
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
func (e *Engine) execBranch(ctx context.Context, rs *runState, branchID string, startEdge *ir.Edge, parentOutputs map[string]map[string]any, parentArtifacts map[string]map[string]any, convergenceNodeID string, slot *branchSlot) *branchResult {
	result := initBranchResult(rs, branchID)
	runID := rs.runID

	// branchCostUSD is this branch's cumulative LLM spend, recorded into the
	// shared daily-cap ledger under ledgerKey. The key carries a per-invocation
	// sequence ("<runID>#<branchID>#<seq>") so concurrent branches don't
	// clobber each other's monotonic-max entry AND a fan-out re-run inside a
	// loop gets a fresh key each iteration (branchID alone repeats across
	// iterations, which would make the monotonic-max keep only the costliest
	// one instead of summing) — see recordBranchUsage.
	var branchCostUSD float64
	ledgerKey := fmt.Sprintf("%s#%s#%d", runID, branchID, rs.branchLedgerSeq.Add(1))

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

		// Bounded-iteration edges inside execBranch are skipped (see
		// edges.go / C243), so iteration here reflects the parent loop
		// counters only.
		iter := e.currentLoopIteration(currentNodeID, rs.loopCounters)

		// Emit node_started.
		if err := e.emitBranch(ctx, runID, branchID, store.EventNodeStarted, currentNodeID, map[string]any{
			"kind":      node.NodeKind().String(),
			"iteration": iter,
		}); err != nil {
			e.logger.Warn("branch %s: failed to emit node_started: %v", branchID, err)
			result.eventErrors++
		}

		output, done := e.executeNodeForBranch(ctx, rs, runID, branchID, currentNodeID, node, parentOutputs, parentArtifacts, iter, result, slot)
		if done {
			return result
		}

		if e.recordBranchUsage(ctx, rs, runID, branchID, ledgerKey, currentNodeID, output, &branchCostUSD, result) {
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
		outputs:          make(map[string]map[string]any),
		artifacts:        make(map[string]map[string]any),
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
	data := map[string]any{}
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
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]any{
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
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]any{
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
func (e *Engine) executeNodeForBranch(ctx context.Context, rs *runState, runID, branchID, currentNodeID string, node ir.Node, parentOutputs, parentArtifacts map[string]map[string]any, iter int, result *branchResult, slot *branchSlot) (map[string]any, bool) {
	merged := mergeOutputs(parentOutputs, result.outputs)
	mergedArt := mergeOutputs(parentArtifacts, result.artifacts)
	branchScope := resolveScope{
		vars:      rs.vars,
		outputs:   merged,
		runInputs: rs.runInputs,
		artifacts: mergedArt,
		rs:        rs,
	}

	// Executor-less special nodes (compute / subbot / emit / wait) are
	// engine-special, not executor-backed — they share the SAME body helpers
	// the main loop uses (ADR-051 "unify branch/main special-node dispatch"),
	// resolved against this branch's merged scope. They carry no `needs:`
	// resource lease here (subbot acquires its own inside runSubbotNode) and
	// never shell out, so they bypass the resource-lease + executor path below;
	// the surrounding execBranch loop still emits their node_started/finished
	// pair and runs publishBranchArtifact. A wait parked here holds this
	// branch's semaphore slot only until the emitting branch fires the event
	// (or the mandatory timeout) — released on park, see launchBranches.
	if output, done, handled := e.executeSpecialNodeForBranch(ctx, rs, branchID, currentNodeID, node, branchScope, result, slot); handled {
		return output, done
	}

	nodeInput := e.buildNodeInputRS(currentNodeID, branchScope)

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
		if emitErr := e.emitBranch(ctx, runID, branchID, store.EventNodeFinished, currentNodeID, map[string]any{
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
func (e *Engine) recordBranchUsage(ctx context.Context, rs *runState, runID, branchID, ledgerKey, currentNodeID string, output map[string]any, branchCostUSD *float64, result *branchResult) bool {
	tokens, costUSD := extractUsage(output)

	if e.dailyCap != nil && costUSD > 0 {
		*branchCostUSD += costUSD
		if _, err := e.dailyCap.Record(ctx, ledgerKey, *branchCostUSD); err != nil {
			e.logger.Warn("branch %s: daily spend cap record failed: %v", branchID, err)
		}
	}

	if rs.budget == nil {
		return false
	}
	checks := rs.budget.RecordUsage(tokens, costUSD)

	for _, w := range findWarnings(checks) {
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetWarning, currentNodeID, map[string]any{
			"dimension": w.dimension,
			"advisory":  w.advisory,
			"used":      w.used,
			"limit":     w.limit,
		}); err != nil {
			e.logger.Warn("branch %s: failed to emit budget_warning: %v", branchID, err)
			result.eventErrors++
		}
	}

	if exc := findExceeded(checks); exc != nil {
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]any{
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
func (e *Engine) publishBranchArtifact(ctx context.Context, runID, branchID, currentNodeID string, node ir.Node, output map[string]any, result *branchResult) bool {
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
	if err := e.emitBranch(ctx, runID, branchID, store.EventArtifactWritten, currentNodeID, map[string]any{
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
func (e *Engine) selectEdgeBranch(ctx context.Context, runID, branchID, fromNodeID string, output map[string]any) (string, error) {
	selected := e.evaluateEdges(fromNodeID, fmt.Sprintf("branch %s", branchID), output)
	if selected == nil {
		return "", fmt.Errorf("no outgoing edge from node %q in branch %s", fromNodeID, branchID)
	}

	if err := e.emitBranch(ctx, runID, branchID, store.EventEdgeSelected, "", map[string]any{
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
