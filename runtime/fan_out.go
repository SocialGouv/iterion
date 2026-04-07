package runtime

import (
	"context"
	"fmt"
	"log"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/store"
)

// ---------------------------------------------------------------------------
// Fan-out / Join — parallel branch scheduler
// ---------------------------------------------------------------------------

// execFanOut handles a fan_out_all router node by spawning parallel
// branches for each outgoing edge, bounded by MaxParallelBranches.
// It returns the next node ID to continue from (after the join).
func (e *Engine) execFanOut(ctx context.Context, rs *runState, routerNodeID string) (string, error) {
	// Emit router node_started.
	if err := e.emit(rs.runID, store.EventNodeStarted, routerNodeID, map[string]interface{}{
		"kind": "router",
		"mode": "fan_out_all",
	}); err != nil {
		return "", err
	}

	// Router is a pass-through: its output = its input from incoming edges.
	routerInput := e.buildNodeInput(routerNodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts)
	rs.outputs[routerNodeID] = routerInput

	// Emit router node_finished.
	if err := e.emit(rs.runID, store.EventNodeFinished, routerNodeID, nil); err != nil {
		return "", err
	}

	// Collect all outgoing edges from the router.
	var fanEdges []*ir.Edge
	for _, edge := range e.workflow.Edges {
		if edge.From == routerNodeID {
			fanEdges = append(fanEdges, edge)
		}
	}
	if len(fanEdges) == 0 {
		return "", fmt.Errorf("fan_out_all router %q has no outgoing edges", routerNodeID)
	}

	// Validate workspace safety: at most one mutating branch in parallel.
	if err := e.validateWorkspaceSafety(fanEdges); err != nil {
		return "", err
	}

	// Determine concurrency limit from budget.
	maxParallel := len(fanEdges)
	if e.workflow.Budget != nil && e.workflow.Budget.MaxParallelBranches > 0 && e.workflow.Budget.MaxParallelBranches < maxParallel {
		maxParallel = e.workflow.Budget.MaxParallelBranches
	}

	// Pre-compute convergence point so branches know where to stop.
	preComputedConvergence := e.findConvergencePoint(routerNodeID, fanEdges)

	// Deep-copy parent outputs and artifacts so branches can't mutate shared state.
	parentOutputs := copyOutputs(rs.outputs)
	parentArtifacts := copyOutputs(rs.artifacts)

	// Launch branches with bounded concurrency.
	sem := make(chan struct{}, maxParallel)
	resultsCh := make(chan *branchResult, len(fanEdges))

	for _, edge := range fanEdges {
		branchID := fmt.Sprintf("branch_%s_%s", routerNodeID, edge.To)

		go func(edge *ir.Edge, branchID string) {
			sem <- struct{}{}        // acquire semaphore slot
			defer func() { <-sem }() // release

			// Recover from panics so a single branch doesn't crash the process.
			defer func() {
				if r := recover(); r != nil {
					resultsCh <- &branchResult{
						branchID: branchID,
						outputs:  make(map[string]map[string]interface{}),
						err:      fmt.Errorf("panic in branch %s: %v", branchID, r),
					}
				}
			}()

			result := e.execBranch(ctx, rs, branchID, edge, parentOutputs, parentArtifacts, preComputedConvergence)
			resultsCh <- result
		}(edge, branchID)
	}

	// Collect all results.
	results := make([]*branchResult, 0, len(fanEdges))
	for range fanEdges {
		results = append(results, <-resultsCh)
	}

	// Determine convergence point. Prefer the one reported by successful branches;
	// if all branches failed, discover it from the graph topology.
	convergenceNodeID := ""
	for _, r := range results {
		if r.joinNodeID != "" {
			if convergenceNodeID == "" {
				convergenceNodeID = r.joinNodeID
			} else if convergenceNodeID != r.joinNodeID {
				return "", fmt.Errorf("branches converge to different nodes: %s vs %s", convergenceNodeID, r.joinNodeID)
			}
		}
	}
	if convergenceNodeID == "" {
		// All branches failed before reaching convergence. Use the
		// pre-computed convergence point (already computed above).
		convergenceNodeID = preComputedConvergence
		if convergenceNodeID == "" {
			return "", fmt.Errorf("no convergence point found after fan_out from %s", routerNodeID)
		}
	}

	// Process convergence.
	return e.processConvergence(rs, convergenceNodeID, results)
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
}

// execBranch runs a single parallel branch starting from the target of
// the given edge. It executes nodes sequentially until it reaches a
// convergence point, a terminal node, or encounters an error.
// convergenceNodeID is the pre-computed convergence point (may be empty
// if unknown; in that case, AwaitMode on individual nodes is checked).
func (e *Engine) execBranch(ctx context.Context, rs *runState, branchID string, startEdge *ir.Edge, parentOutputs map[string]map[string]interface{}, parentArtifacts map[string]map[string]interface{}, convergenceNodeID string) *branchResult {
	result := &branchResult{
		branchID:         branchID,
		outputs:          make(map[string]map[string]interface{}),
		artifacts:        make(map[string]map[string]interface{}),
		artifactVersions: make(map[string]int),
	}

	runID := rs.runID
	vars := rs.vars
	runInputs := rs.runInputs

	// Emit branch_started (best-effort — branch can proceed without the event).
	if err := e.emitBranch(runID, branchID, store.EventBranchStarted, startEdge.To, nil); err != nil {
		log.Printf("runtime: branch %s: failed to emit branch_started: %v", branchID, err)
		result.eventErrors++
	}

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
			return result
		case *ir.FailNode:
			result.err = fmt.Errorf("branch %s reached fail node %q", branchID, currentNodeID)
			return result
		}

		// Check budget before execution.
		if rs.budget != nil {
			checks := rs.budget.Check()
			if exc := findExceeded(checks); exc != nil {
				if err := e.emitBranch(runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]interface{}{
					"dimension": exc.dimension,
					"used":      exc.used,
					"limit":     exc.limit,
				}); err != nil {
					log.Printf("runtime: branch %s: failed to emit budget_exceeded: %v", branchID, err)
					result.eventErrors++
				}
				result.err = fmt.Errorf("%w: %s (%.0f/%.0f)", ErrBudgetExceeded, exc.dimension, exc.used, exc.limit)
				return result
			}
			if hl := findHardLimited(checks); hl != nil {
				if err := e.emitBranch(runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]interface{}{
					"dimension":  hl.dimension,
					"used":       hl.used,
					"limit":      hl.limit,
					"hard_limit": true,
				}); err != nil {
					log.Printf("runtime: branch %s: failed to emit budget hard limit: %v", branchID, err)
					result.eventErrors++
				}
				result.err = fmt.Errorf("%w: hard limit %s at %.0f%% (%.0f/%.0f)", ErrBudgetExceeded, hl.dimension, (hl.used/hl.limit)*100, hl.used, hl.limit)
				return result
			}
		}

		// Emit node_started.
		if err := e.emitBranch(runID, branchID, store.EventNodeStarted, currentNodeID, map[string]interface{}{
			"kind": node.NodeKind().String(),
		}); err != nil {
			log.Printf("runtime: branch %s: failed to emit node_started: %v", branchID, err)
			result.eventErrors++
		}

		// Build input: merge parent outputs with branch-local outputs so
		// refs to upstream nodes (before the router) still resolve.
		merged := mergeOutputs(parentOutputs, result.outputs)
		mergedArt := mergeOutputs(parentArtifacts, result.artifacts)
		nodeInput := e.buildNodeInput(currentNodeID, vars, merged, runInputs, mergedArt)

		// Execute.
		output, err := e.executor.Execute(ctx, node, nodeInput)
		if err != nil {
			result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, err)
			if emitErr := e.emitBranch(runID, branchID, store.EventNodeFinished, currentNodeID, map[string]interface{}{
				"error": err.Error(),
			}); emitErr != nil {
				log.Printf("runtime: branch %s: failed to emit node_finished: %v", branchID, emitErr)
				result.eventErrors++
			}
			return result
		}

		result.outputs[currentNodeID] = output

		// Validate output against declared schema (optional).
		if err := e.validateNodeOutput(currentNodeID, node, output); err != nil {
			result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, err)
			return result
		}

		// Record budget usage and check limits.
		if rs.budget != nil {
			tokens, costUSD := extractUsage(output)
			checks := rs.budget.RecordUsage(tokens, costUSD)

			// Emit warnings.
			for _, w := range findWarnings(checks) {
				if err := e.emitBranch(runID, branchID, store.EventBudgetWarning, currentNodeID, map[string]interface{}{
					"dimension": w.dimension,
					"used":      w.used,
					"limit":     w.limit,
				}); err != nil {
					log.Printf("runtime: branch %s: failed to emit budget_warning: %v", branchID, err)
					result.eventErrors++
				}
			}

			// Fail on exceeded.
			if exc := findExceeded(checks); exc != nil {
				if err := e.emitBranch(runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]interface{}{
					"dimension": exc.dimension,
					"used":      exc.used,
					"limit":     exc.limit,
				}); err != nil {
					log.Printf("runtime: branch %s: failed to emit budget_exceeded: %v", branchID, err)
					result.eventErrors++
				}
				result.err = fmt.Errorf("%w: %s (%.0f/%.0f)", ErrBudgetExceeded, exc.dimension, exc.used, exc.limit)
				return result
			}
		}

		// Persist artifact if node has publish.
		if pub := nodePublish(node); pub != "" {
			version := result.artifactVersions[currentNodeID]
			artifact := &store.Artifact{
				RunID:   runID,
				NodeID:  currentNodeID,
				Version: version,
				Data:    output,
			}
			if err := e.store.WriteArtifact(artifact); err != nil {
				result.err = fmt.Errorf("node %q in branch %s: write artifact: %w", currentNodeID, branchID, err)
				return result
			}
			result.artifactVersions[currentNodeID] = version + 1
			result.artifacts[pub] = output
			if err := e.emitBranch(runID, branchID, store.EventArtifactWritten, currentNodeID, map[string]interface{}{
				"publish": pub,
				"version": version,
			}); err != nil {
				log.Printf("runtime: branch %s: failed to emit artifact_written: %v", branchID, err)
				result.eventErrors++
			}
		}

		// Emit node_finished with usage data.
		if err := e.emitBranch(runID, branchID, store.EventNodeFinished, currentNodeID, buildNodeFinishedData(output)); err != nil {
			log.Printf("runtime: branch %s: failed to emit node_finished: %v", branchID, err)
			result.eventErrors++
		}
		if e.onNodeFinished != nil {
			e.onNodeFinished(currentNodeID, output)
		}

		// Select next edge (branch-local, no loop counters needed in branches).
		nextNodeID, err := e.selectEdgeBranch(runID, branchID, currentNodeID, output)
		if err != nil {
			result.err = err
			return result
		}

		currentNodeID = nextNodeID
	}
}

// selectEdgeBranch picks the next node for a branch. It is simpler than
// selectEdge: no loop counter enforcement, events carry a branch ID.
func (e *Engine) selectEdgeBranch(runID, branchID, fromNodeID string, output map[string]interface{}) (string, error) {
	selected := e.evaluateEdges(fromNodeID, fmt.Sprintf("branch %s", branchID), output)
	if selected == nil {
		return "", fmt.Errorf("no outgoing edge from node %q in branch %s", fromNodeID, branchID)
	}

	if err := e.emitBranch(runID, branchID, store.EventEdgeSelected, "", map[string]interface{}{
		"from": selected.From,
		"to":   selected.To,
	}); err != nil {
		log.Printf("runtime: branch %s: failed to emit edge_selected: %v", branchID, err)
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
	if err := e.emit(rs.runID, store.EventJoinReady, convergenceNodeID, convData); err != nil {
		log.Printf("runtime: failed to emit convergence_ready: %v", err)
	}

	// Return the convergence node ID — the main loop will execute it normally.
	return convergenceNodeID, nil
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
	for _, startEdge := range fanEdges {
		visited := map[string]bool{}
		queue := []string{startEdge.To}
		for len(queue) > 0 {
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

// copyOutputs creates a two-level copy of the outputs map so that concurrent
// branches cannot mutate shared parent state at the top two levels.
func copyOutputs(src map[string]map[string]interface{}) map[string]map[string]interface{} {
	dst := make(map[string]map[string]interface{}, len(src))
	for k, v := range src {
		inner := make(map[string]interface{}, len(v))
		for ik, iv := range v {
			inner[ik] = iv
		}
		dst[k] = inner
	}
	return dst
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

// branchContainsMutation walks from startNodeID to a join/terminal node
// and returns true if any node along the path may mutate the workspace.
func (e *Engine) branchContainsMutation(startNodeID string) bool {
	visited := map[string]bool{}
	queue := []string{startNodeID}
	for len(queue) > 0 {
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
		// Stop walking at convergence points or terminal nodes.
		if nodeAwaitMode(node) != ir.AwaitNone || isTerminalNode(node) {
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
func (e *Engine) validateWorkspaceSafety(fanEdges []*ir.Edge) error {
	mutatingCount := 0
	var mutatingBranches []string
	for _, edge := range fanEdges {
		if e.branchContainsMutation(edge.To) {
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
