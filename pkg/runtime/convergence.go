package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// processConvergence aggregates branch results according to the convergence
// node's await strategy, merges outputs into the run state, builds the
// convergence node's input from multi-edge with-mappings, and returns
// the convergence node ID for the main loop to continue execution.
func (e *Engine) processConvergence(rs *runState, convergenceNodeID string, results []*branchResult) (string, error) {
	convNode, ok := e.workflow.Nodes[convergenceNodeID]
	if !ok {
		return "", &RuntimeError{Code: ErrCodeNodeNotFound, NodeID: convergenceNodeID, Message: fmt.Sprintf("convergence node %q not found", convergenceNodeID)}
	}

	// Determine await strategy: use node's explicit setting, default to wait_all.
	strategy := nodeAwaitMode(convNode)
	if strategy == ir.AwaitNone {
		strategy = ir.AwaitWaitAll
	}

	// Collect failed branches metadata.
	var failedBranches []map[string]any
	// budgetFailures/otherFailures classify why the branches died. A budget
	// refusal cancels its siblings (cancelOnFirstFailure), so a fan-out killed
	// by a spent budget yields one budget error plus N cancellations — the
	// cancellations carry no verdict of their own and must not mask it.
	budgetFailures, otherFailures := 0, 0
	var firstBudgetErr error
	for _, r := range results {
		if r.err != nil {
			failedBranches = append(failedBranches, map[string]any{
				"branch_id": r.branchID,
				"error":     r.err.Error(),
			})
			switch {
			case errors.Is(r.err, ErrBudgetExceeded):
				budgetFailures++
				if firstBudgetErr == nil {
					firstBudgetErr = r.err
				}
			case errors.Is(r.err, context.Canceled), errors.Is(r.err, context.DeadlineExceeded), errors.Is(r.err, ErrRunCancelled):
			default:
				otherFailures++
			}
		}
	}

	// Apply await strategy.
	switch strategy {
	case ir.AwaitWaitAll:
		if len(failedBranches) > 0 {
			if budgetFailures > 0 && otherFailures == 0 {
				// A branch never gets the exit grace (withinBudgetGrace), so a
				// fan-out reached on a spent budget refuses every branch and
				// wait_all kills the run here. That death has to keep carrying
				// the sentinel: the cloud runner's terminal-ack carve-out
				// matches errors.Is(err, ErrBudgetExceeded), and a naked error
				// goes back to JetStream as retryable — a resume/refail loop
				// re-provisioning a sandbox to re-hit the same spent budget.
				// It is also what tells the operator to raise the cap and
				// resume rather than hunt a branch bug.
				return "", &RuntimeError{
					Code:    ErrCodeBudgetExceeded,
					Message: fmt.Sprintf("convergence at %s (wait_all): %d of %d branch(es) refused on a spent budget: %v", convergenceNodeID, budgetFailures, len(failedBranches), firstBudgetErr),
					NodeID:  convergenceNodeID,
					Hint:    "raise the exceeded budget dimension (--max-cost-usd / --max-tokens / --max-duration / --max-iterations) and resume; a parallel branch never receives the budget exit grace",
					Cause:   ErrBudgetExceeded,
				}
			}
			msg := fmt.Sprintf("convergence at %s (wait_all): %d branch(es) failed: %v",
				convergenceNodeID, len(failedBranches), failedBranches[0]["error"])
			// When every failed branch carries the SAME typed code, the
			// aggregate keeps it — a fan-out hitting one deterministic
			// wall (a ghost node after a source edit) must not launder
			// NODE_NOT_FOUND into the EXECUTION_FAILED catch-all — the
			// in-process auto-resume gate retries the latter and
			// refuses the former.
			if code := commonBranchFailureCode(results); code != "" {
				return "", &RuntimeError{Code: code, NodeID: convergenceNodeID, Message: msg}
			}
			return "", fmt.Errorf("%s", msg)
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
			// Max-merge (not last-write-wins): every branch copies the full
			// parent version map, so a branch that never touched this node
			// still carries its pre-fan-out version. A plain assignment would
			// let a stale branch ordered after the publishing one regress the
			// counter — freezing the persisted artifacts/<node>/<v>.json
			// history on a looped fan-out. Keep the highest version reached.
			if version > rs.artifactVersions[nodeID] {
				rs.artifactVersions[nodeID] = version
			}
		}
	}

	// Add failed branches metadata to outputs so it's available via with-mappings.
	if len(failedBranches) > 0 {
		// Expose as a special output on the convergence node.
		if rs.outputs[convergenceNodeID] == nil {
			rs.outputs[convergenceNodeID] = make(map[string]any)
		}
		rs.outputs[convergenceNodeID]["_failed_branches"] = failedBranches
	}

	// Emit convergence_ready event.
	convData := map[string]any{
		"strategy": strategy.String(),
	}
	if len(failedBranches) > 0 {
		convData["failed_branches"] = failedBranches
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventJoinReady, convergenceNodeID, convData); err != nil {
		e.logger.Warn("failed to emit convergence_ready: %v", err)
	}

	mergeJoinIncoming(rs, convergenceNodeID, results)

	// Return the convergence node ID — the main loop will execute it normally.
	return convergenceNodeID, nil
}

// processConvergenceTerminal handles an all-done topology (every branch ran
// to its own *ir.DoneNode and no branch failed), including an implicit
// wait_all fan whose bounded local cycle leaves no structural collector.
// Merges branch outputs/artifacts into the run state and hands back one
// of the terminal node IDs so the engine's main loop emits run_finished.
func (e *Engine) processConvergenceTerminal(rs *runState, results []*branchResult, strategy ir.AwaitMode) (string, error) {
	for _, r := range results {
		for nodeID, output := range r.outputs {
			rs.outputs[nodeID] = output
		}
		for name, output := range r.artifacts {
			rs.artifacts[name] = output
		}
		for nodeID, version := range r.artifactVersions {
			// Max-merge (not last-write-wins): every branch copies the full
			// parent version map, so a branch that never touched this node
			// still carries its pre-fan-out version. A plain assignment would
			// let a stale branch ordered after the publishing one regress the
			// counter — freezing the persisted artifacts/<node>/<v>.json
			// history on a looped fan-out. Keep the highest version reached.
			if version > rs.artifactVersions[nodeID] {
				rs.artifactVersions[nodeID] = version
			}
		}
	}
	// Use the first branch's terminal node — the engine treats any Done
	// node as run_finished, so picking one is unambiguous.
	terminal := results[0].terminalNodeID
	if err := e.emit(rs.ctx, rs.runID, store.EventJoinReady, terminal, map[string]any{
		"strategy":       strategy.String(),
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
		// A branch-local loop back-edge is control flow inside one branch,
		// never evidence that its head is a cross-branch collector.
		if edge.IsBoundedIteration() {
			continue
		}
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
func mergeOutputs(parent, branch map[string]map[string]any) map[string]map[string]any {
	merged := make(map[string]map[string]any, len(parent)+len(branch))
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
func copyOutputs(src map[string]map[string]any) map[string]map[string]any {
	dst := make(map[string]map[string]any, len(src))
	for k, v := range src {
		inner := make(map[string]any, len(v))
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
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopyValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopyValue(val)
		}
		return out
	default:
		return v
	}
}

// commonBranchFailureCode returns the typed failure code shared by
// EVERY failed branch result, or "" when they disagree (or none is
// typed) — partial agreement stays the catch-all, never a guess.
func commonBranchFailureCode(results []*branchResult) ErrorCode {
	var code ErrorCode
	for _, r := range results {
		if r == nil || r.err == nil {
			continue
		}
		var rtErr *RuntimeError
		if !errors.As(r.err, &rtErr) || rtErr.Code == "" {
			return ""
		}
		if code == "" {
			code = rtErr.Code
		} else if code != rtErr.Code {
			return ""
		}
	}
	return code
}
