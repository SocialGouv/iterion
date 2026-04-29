package runtime

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Round-robin router
// ---------------------------------------------------------------------------

// execRoundRobin handles a round_robin router node by selecting a single
// outgoing edge based on a cyclical counter. Unlike fan_out_all, it does
// not spawn parallel branches — it picks one target and returns to the
// main execution loop.
func (e *Engine) execRoundRobin(ctx context.Context, rs *runState, routerNodeID string) (string, error) {
	// Collect unconditional outgoing edges from the router.
	var edges []*ir.Edge
	for _, edge := range e.workflow.Edges {
		if edge.From == routerNodeID && !edge.IsConditional() {
			edges = append(edges, edge)
		}
	}
	if len(edges) == 0 {
		return "", fmt.Errorf("round_robin router %q has no outgoing edges", routerNodeID)
	}

	// Cyclical selection: counter % len(edges).
	counter := rs.roundRobinCounters[routerNodeID]
	selected := edges[counter%len(edges)]
	rs.roundRobinCounters[routerNodeID] = counter + 1

	// Clear stale outputs from sibling targets not selected this round.
	// Without this, buildNodeInput would pick up with-mappings from edges
	// whose source ran in a previous iteration, causing downstream nodes
	// to receive stale data.
	for _, edge := range edges {
		if edge.To != selected.To {
			delete(rs.outputs, edge.To)
		}
	}

	// Emit router node_started with round-robin metadata.
	if err := e.emit(rs.runID, store.EventNodeStarted, routerNodeID, map[string]interface{}{
		"kind":              "router",
		"mode":              "round_robin",
		"round_robin_index": counter,
		"selected_target":   selected.To,
	}); err != nil {
		return "", err
	}

	// Router is a pass-through: its output = its input from incoming edges.
	routerInput := e.buildNodeInputRS(routerNodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts, rs)
	rs.outputs[routerNodeID] = routerInput

	// Emit router node_finished.
	if err := e.emit(rs.runID, store.EventNodeFinished, routerNodeID, nil); err != nil {
		return "", err
	}

	return selected.To, nil
}

// ---------------------------------------------------------------------------
// LLM Router — LLM-based route selection
// ---------------------------------------------------------------------------

// execLLMRouter handles an LLM router node by calling the LLM to decide
// which outgoing edge(s) to take. For single mode, it picks one target;
// for multi mode, it fans out to the selected subset.
func (e *Engine) execLLMRouter(ctx context.Context, rs *runState, routerNodeID string) (string, error) {
	node := e.workflow.Nodes[routerNodeID]
	rn, ok := node.(*ir.RouterNode)
	if !ok {
		return "", fmt.Errorf("runtime: node %q is not a RouterNode", routerNodeID)
	}

	// Emit node_started.
	if err := e.emit(rs.runID, store.EventNodeStarted, routerNodeID, map[string]interface{}{
		"kind": "router",
		"mode": "llm",
	}); err != nil {
		return "", err
	}

	// Build router input.
	routerInput := e.buildNodeInputRS(routerNodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts, rs)

	// Collect outgoing edge targets as candidates.
	// NOTE: order follows edge declaration order in the .iter file, which the
	// LLM sees in its prompt. This is deterministic but may introduce ordering
	// bias in the LLM's selection.
	var candidates []string
	for _, edge := range e.workflow.Edges {
		if edge.From == routerNodeID {
			candidates = append(candidates, edge.To)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("llm router %q has no outgoing edges", routerNodeID)
	}

	// Inject candidates into input for the executor.
	routerInput["_route_candidates"] = candidates

	// Check budget before LLM call.
	if err := e.checkBudgetBeforeExec(rs, routerNodeID); err != nil {
		return "", err
	}

	// Execute LLM call via the executor.
	output, err := e.executor.Execute(ctx, node, routerInput)
	if err != nil {
		return "", fmt.Errorf("llm router %q: %w", routerNodeID, err)
	}

	rs.outputs[routerNodeID] = output

	// Record budget usage and check limits.
	if err := e.recordAndCheckBudget(rs, routerNodeID, output); err != nil {
		return "", err
	}

	// Dispatch based on single/multi mode.
	if rn.RouterMulti {
		return e.execLLMRouterMulti(ctx, rs, routerNodeID, output, candidates)
	}
	return e.execLLMRouterSingle(rs, routerNodeID, output, candidates)
}

// execLLMRouterSingle handles single-route LLM selection.
func (e *Engine) execLLMRouterSingle(rs *runState, routerNodeID string, output map[string]interface{}, candidates []string) (string, error) {
	selected, ok := output["selected_route"].(string)
	if !ok || selected == "" {
		return "", &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("llm router %q did not produce a valid selected_route", routerNodeID),
			NodeID:  routerNodeID,
		}
	}

	// Validate selection is a valid candidate.
	valid := false
	for _, c := range candidates {
		if c == selected {
			valid = true
			break
		}
	}
	if !valid {
		return "", &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("llm router %q selected %q which is not a valid target (candidates: %v)", routerNodeID, selected, candidates),
			NodeID:  routerNodeID,
		}
	}

	reasoning, _ := output["reasoning"].(string)

	// Emit node_finished.
	if err := e.emit(rs.runID, store.EventNodeFinished, routerNodeID, map[string]interface{}{
		"selected_route": selected,
		"reasoning":      reasoning,
	}); err != nil {
		return "", err
	}

	// Emit edge_selected.
	if err := e.emit(rs.runID, store.EventEdgeSelected, routerNodeID, map[string]interface{}{
		"from": routerNodeID,
		"to":   selected,
	}); err != nil {
		return "", err
	}

	return selected, nil
}

// execLLMRouterMulti handles multi-route LLM selection by fanning out
// to the LLM-selected subset of outgoing edges.
func (e *Engine) execLLMRouterMulti(ctx context.Context, rs *runState, routerNodeID string, output map[string]interface{}, candidates []string) (string, error) {
	selectedRaw, ok := output["selected_routes"]
	if !ok {
		return "", &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("llm router %q did not produce selected_routes", routerNodeID),
			NodeID:  routerNodeID,
		}
	}

	// Parse selected routes from the output.
	var selected []string
	switch v := selectedRaw.(type) {
	case []interface{}:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return "", &RuntimeError{
					Code:    ErrCodeExecutionFailed,
					Message: fmt.Sprintf("llm router %q: selected_routes contains non-string element", routerNodeID),
					NodeID:  routerNodeID,
				}
			}
			selected = append(selected, s)
		}
	case []string:
		selected = v
	default:
		return "", &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("llm router %q: selected_routes is %T, expected array", routerNodeID, selectedRaw),
			NodeID:  routerNodeID,
		}
	}

	if len(selected) == 0 {
		return "", &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("llm router %q selected zero routes", routerNodeID),
			NodeID:  routerNodeID,
		}
	}

	// Validate all selections are valid candidates.
	candidateSet := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		candidateSet[c] = true
	}
	for _, s := range selected {
		if !candidateSet[s] {
			return "", &RuntimeError{
				Code:    ErrCodeExecutionFailed,
				Message: fmt.Sprintf("llm router %q selected %q which is not a valid target (candidates: %v)", routerNodeID, s, candidates),
				NodeID:  routerNodeID,
			}
		}
	}

	reasoning, _ := output["reasoning"].(string)

	// Emit node_finished.
	if err := e.emit(rs.runID, store.EventNodeFinished, routerNodeID, map[string]interface{}{
		"selected_routes": selected,
		"reasoning":       reasoning,
	}); err != nil {
		return "", err
	}

	// Filter workflow edges to only the LLM-selected targets.
	selectedSet := make(map[string]bool, len(selected))
	for _, s := range selected {
		selectedSet[s] = true
	}
	var fanEdges []*ir.Edge
	for _, edge := range e.workflow.Edges {
		if edge.From == routerNodeID && selectedSet[edge.To] {
			fanEdges = append(fanEdges, edge)
		}
	}

	// Validate workspace safety.
	if err := e.validateWorkspaceSafety(fanEdges); err != nil {
		return "", err
	}

	// Determine concurrency limit from budget.
	maxParallel := len(fanEdges)
	if e.workflow.Budget != nil && e.workflow.Budget.MaxParallelBranches > 0 && e.workflow.Budget.MaxParallelBranches < maxParallel {
		maxParallel = e.workflow.Budget.MaxParallelBranches
	}

	// Pre-compute convergence point so branches know where to stop.
	llmPreComputedConvergence := e.findConvergencePoint(routerNodeID, fanEdges)

	// Deep-copy parent outputs and artifacts so branches can't mutate shared state.
	parentOutputs := copyOutputs(rs.outputs)
	parentArtifacts := copyOutputs(rs.artifacts)

	// Launch branches with bounded concurrency.
	sem := make(chan struct{}, maxParallel)
	resultsCh := make(chan *branchResult, len(fanEdges))

	for _, edge := range fanEdges {
		branchID := fmt.Sprintf("branch_%s_%s", routerNodeID, edge.To)

		go func(edge *ir.Edge, branchID string) {
			sem <- struct{}{}
			defer func() { <-sem }()

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

			result := e.execBranch(ctx, rs, branchID, edge, parentOutputs, parentArtifacts, llmPreComputedConvergence)
			resultsCh <- result
		}(edge, branchID)
	}

	// Collect all results.
	results := make([]*branchResult, 0, len(fanEdges))
	for range fanEdges {
		results = append(results, <-resultsCh)
	}

	// Determine convergence point.
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
		convergenceNodeID = e.findConvergencePoint(routerNodeID, fanEdges)
		if convergenceNodeID == "" {
			return "", fmt.Errorf("no convergence point found after llm router fan-out from %s", routerNodeID)
		}
	}

	return e.processConvergence(rs, convergenceNodeID, results)
}
