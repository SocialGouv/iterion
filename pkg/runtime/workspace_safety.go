package runtime

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

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
// Subbot nodes run a child .bot that may do anything (including mutate the
// shared worktree), so they are conservatively treated as mutating — this
// keeps validateWorkspaceSafety from admitting two subbot branches that would
// race the same workspace. Nodes with Readonly=true are never considered
// mutating.
func isMutatingNode(node ir.Node) bool {
	switch n := node.(type) {
	case *ir.ToolNode:
		return true
	case *ir.SubbotNode:
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
