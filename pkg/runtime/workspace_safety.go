package runtime

import (
	"fmt"
	"strings"

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
// Tool nodes are always mutating in this general classifier. Agent/judge nodes
// are mutating when full_access is set, when they have at least one tool that is
// not in the read-only set, or when the effective backend is a CLI delegate and
// its tool list is omitted (CLI delegates treat an empty list as unrestricted
// native tools). The engine asks the production executor for the effective
// backend so launch overrides, environment defaults, and auto-detection are
// included. Subbot nodes run a child .bot that may do anything (including mutate
// the shared worktree), so they are conservatively treated as mutating — this
// keeps validateWorkspaceSafety from admitting two subbot branches that would
// race the same workspace. A subbot may opt out with `isolated:` (it asserts
// the child confines writes to its own run store / worktree) and an agent/judge
// node with Readonly=true; both are context-independent, so they are honoured
// everywhere. A tool's `parallel_safe:` opt-out is NOT honoured here: it only
// applies to a fan_out_each template (see isMutatingNodeCtx), because only there
// is a single node replayed over distinct items with disjoint, item-keyed
// writes — in a static fan_out_all / llm-router the branches are different nodes
// with no such guarantee.
func isMutatingNode(node ir.Node) bool {
	return isMutatingNodeWithBackend(node, "", nil)
}

// effectiveBackendResolver is implemented by the production model executor.
// Keeping the interface here avoids duplicating its evolving resolution chain
// (launch override -> DSL -> workflow default -> env -> auto-detection).
type effectiveBackendResolver interface {
	EffectiveBackendName(ir.Node) string
}

func isMutatingNodeWithBackend(node ir.Node, defaultBackend string, resolver effectiveBackendResolver) bool {
	return isMutatingNodeCtx(node, defaultBackend, resolver, false)
}

// isMutatingNodeCtx classifies a node for workspace-safety. fanOutEachTemplate is
// true only when walking a fan_out_each template branch; there a tool marked
// `parallel_safe:` is treated as non-mutating (the author asserts its concurrent
// replays write only to disjoint, item-keyed targets — mirror of a subbot's
// `isolated:` / an agent-judge `readonly:`, but scoped to the fan_out_each
// replay, the only place a single template node is fanned out over items). In
// every other context (fanOutEachTemplate=false) a tool is conservatively
// mutating even with the flag: static fan_out_all / llm-router branches are
// DISTINCT nodes with no item-key disjointness guarantee. Subbot Isolated and
// agent/judge Readonly are context-independent and honoured in both.
func isMutatingNodeCtx(node ir.Node, defaultBackend string, resolver effectiveBackendResolver, fanOutEachTemplate bool) bool {
	switch n := node.(type) {
	case *ir.ToolNode:
		return !fanOutEachTemplate || !n.ParallelSafe
	case *ir.SubbotNode:
		// A subbot marked `isolated:` asserts the child confines its writes to
		// its own run store / worktree and never touches the parent's shared
		// workspace, so it is safe to fan out in parallel (mirror of an
		// agent/judge node's `readonly:`). Absent the flag, the child may do
		// anything — conservatively mutating.
		return !n.Isolated
	case *ir.AgentNode:
		if n.Readonly {
			return false
		}
		if n.FullAccess || unrestrictedCLIBackendCanWrite(node, n.LLMFields, n.Tools, defaultBackend, resolver) {
			return true
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
		if n.FullAccess || unrestrictedCLIBackendCanWrite(node, n.LLMFields, n.Tools, defaultBackend, resolver) {
			return true
		}
		for _, t := range n.Tools {
			if !readOnlyTools[t] {
				return true
			}
		}
	}
	return false
}

func unrestrictedCLIBackendCanWrite(
	node ir.Node,
	fields ir.LLMFields,
	tools []string,
	defaultBackend string,
	resolver effectiveBackendResolver,
) bool {
	if len(tools) > 0 {
		return false
	}
	backend := strings.TrimSpace(ir.ExpandEnvWithDefault(fields.Backend))
	if backend == "" {
		backend = strings.TrimSpace(ir.ExpandEnvWithDefault(defaultBackend))
	}
	if resolver != nil {
		if effective := strings.TrimSpace(resolver.EffectiveBackendName(node)); effective != "" {
			backend = effective
		}
	}
	if backend == "" || backend == "auto" {
		return false
	}
	return backend != "claw"
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
//
// fanOutEachTemplate is true only when the branch is a fan_out_each template
// (one node replayed over items); it relaxes a `parallel_safe:` tool to
// non-mutating along the walk. A branch that also contains any OTHER mutating
// node (a non-parallel_safe tool, a full_access agent, a non-isolated subbot)
// is still reported as mutating.
func (e *Engine) branchContainsMutation(startNodeID, globalConvergence string, fanOutEachTemplate bool) bool {
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
		resolver, _ := e.executor.(effectiveBackendResolver)
		if isMutatingNodeCtx(node, e.workflow.DefaultBackend, resolver, fanOutEachTemplate) {
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
		// Static fan_out_all / llm-router: branches are DISTINCT nodes, so a
		// tool's `parallel_safe:` (item-keyed disjoint replays) does not apply —
		// pass fanOutEachTemplate=false.
		if e.branchContainsMutation(edge.To, globalConvergence, false) {
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

// validateFanOutEachWorkspaceSafety applies the same shared-worktree mutation
// guard to data-driven fan-out. A fan_out_each has only one static template
// edge, so the static fan_out_all check cannot see that the template may be
// executed N times concurrently at runtime. Once cardinality and the effective
// parallelism cap are known, reject concurrent replays of a mutating template.
func (e *Engine) validateFanOutEachWorkspaceSafety(routerNodeID string, tmplEdge *ir.Edge, convergence string, itemCount, maxParallel int) error {
	if itemCount <= 1 || maxParallel <= 1 || tmplEdge == nil {
		return nil
	}
	// Fan_out_each template: a single node replayed over items, so a
	// `parallel_safe:` tool along the template is exempt (item-keyed disjoint
	// writes). Any other mutating node still trips the guard.
	if !e.branchContainsMutation(tmplEdge.To, convergence, true) {
		return nil
	}
	return &RuntimeError{
		Code:    ErrCodeWorkspaceSafety,
		Message: fmt.Sprintf("workspace safety violation: fan_out_each router %q would run mutating template branch %q concurrently for %d items", routerNodeID, tmplEdge.To, itemCount),
		Hint:    "mutating fan_out_each templates must run with max_parallel_branches=1 or be moved to sequential steps/read-only nodes",
	}
}
