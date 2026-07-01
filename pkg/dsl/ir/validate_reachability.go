package ir

import (
	"fmt"
)

// ---------------------------------------------------------------------------
// C016 — unreachable nodes
// ---------------------------------------------------------------------------

func (c *compiler) validateReachability(w *Workflow) {
	if w.Entry == "" {
		return
	}

	// Build adjacency list from edges.
	adj := make(map[string][]string)
	for _, e := range w.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	// BFS from entry.
	visited := make(map[string]bool)
	queue := []string{w.Entry}
	visited[w.Entry] = true
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}

	// Report unreachable non-terminal nodes.
	// Skip "done" and "fail" if they have no incoming edges — they're always present.
	for id, node := range w.Nodes {
		if visited[id] {
			continue
		}
		// Terminal nodes are always added; skip them if unreachable — it's fine.
		switch node.(type) {
		case *DoneNode, *FailNode:
			continue
		}
		c.errorfAt(DiagUnreachableNode, id, "",
			"node %q (%s) is unreachable from entry %q",
			id, node.NodeKind(), w.Entry)
	}
}

// ---------------------------------------------------------------------------
// C017 — outputs.<node>.history reference requires node to be in a loop
// ---------------------------------------------------------------------------

func (c *compiler) validateHistoryRefs(w *Workflow) {
	// Build set of nodes that participate in a loop (appear on a loop-bearing edge).
	loopNodes := make(map[string]bool)
	for _, e := range w.Edges {
		if e.LoopName != "" {
			loopNodes[e.From] = true
			loopNodes[e.To] = true
		}
	}

	// Check all refs in prompts and edge with-mappings.
	checkRef := func(ctx string, ref *Ref) {
		if ref.Kind != RefOutputs {
			return
		}
		// outputs.<node>.history pattern: Path = [node, "history"]
		if len(ref.Path) >= 2 && ref.Path[len(ref.Path)-1] == "history" {
			nodeID := ref.Path[0]
			if _, ok := w.Nodes[nodeID]; !ok {
				return // unknown node already reported by other checks
			}
			if !loopNodes[nodeID] {
				c.errorf(DiagHistoryRefNotInLoop,
					"%s: reference %s uses .history but node %q is not in any loop",
					ctx, ref.Raw, nodeID)
			}
		}
	}

	// Check prompts.
	for _, p := range w.Prompts {
		for _, ref := range p.TemplateRefs {
			checkRef(fmt.Sprintf("prompt %q", p.Name), ref)
		}
	}

	// Check edge with-mappings.
	for _, e := range w.Edges {
		for _, dm := range e.With {
			for _, ref := range dm.Refs {
				checkRef(fmt.Sprintf("edge %s -> %s, with %q", e.From, e.To, dm.Key), ref)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// C019 — undeclared cycles (back-edges without a loop declaration)
// ---------------------------------------------------------------------------

// validateUndeclaredCycles uses DFS to detect cycles that have no declared
// loop on any of their edges. Such cycles would cause infinite execution
// if no budget is set.
func (c *compiler) validateUndeclaredCycles(w *Workflow) {
	// Build set of nodes that participate in a declared loop.
	// A cycle is considered bounded if ANY edge in the cycle carries a
	// LoopName — the runtime enforces max_iterations on that edge.
	loopNodes := make(map[string]bool)
	for _, e := range w.Edges {
		// A foreach back-edge is a bounded cycle too — the runtime stops it
		// when the collection is exhausted.
		if e.IsBoundedIteration() {
			loopNodes[e.From] = true
			loopNodes[e.To] = true
		}
	}

	// Build adjacency list.
	adj := make(map[string][]string)
	for _, e := range w.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	// DFS with three-color marking: white (unseen), gray (in stack), black (done).
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int) // default white

	var dfs func(node string)
	dfs = func(node string) {
		color[node] = gray
		for _, to := range adj[node] {
			switch color[to] {
			case gray:
				// Back-edge found — cycle. Only report if neither endpoint
				// participates in a declared loop (which bounds the cycle).
				if !loopNodes[node] && !loopNodes[to] {
					c.errorf(DiagUndeclaredCycle,
						"cycle detected: edge %s -> %s forms a cycle without a declared loop; add a loop with max_iterations to bound it",
						node, to)
				}
			case white:
				dfs(to)
			}
		}
		color[node] = black
	}

	// Walk from Entry first (preserves prior visit order so the same
	// graph still emits the same diagnostic for the same back-edge),
	// then sweep over every other node so cycles in components that
	// aren't directly reachable from Entry are also detected. The
	// reachability check (C016) handles "cycle in unreachable region"
	// separately — both diagnostics may now fire on the same workflow.
	if w.Entry != "" {
		dfs(w.Entry)
	}
	for n := range adj {
		if color[n] == white {
			dfs(n)
		}
	}
}

// ---------------------------------------------------------------------------
// C026 — loop max_iterations must be >= 1
// ---------------------------------------------------------------------------

func (c *compiler) validateLoopIterations(w *Workflow) {
	for _, loop := range w.Loops {
		// Templated caps (`as fix_loop("{{outputs.X.cap}}")`) carry
		// MaxIterations=0 by design — the real bound is resolved at
		// runtime from the referenced output/var. The runtime falls
		// back to MaxIterations (0) if resolution fails, which produces
		// a "loop exhausted on iteration 0" log line: the operator
		// sees the wiring problem without compile-time blocking
		// otherwise-valid templated declarations.
		if loop.MaxIterationsExpr != "" {
			continue
		}
		// Unbounded loops legitimately carry no literal iteration cap; their
		// bound is fuel + liveness (validated by C097 below), so C026 is N/A.
		if loop.Unbounded {
			continue
		}
		if loop.MaxIterations < 1 {
			c.errorf(DiagInvalidLoopIterations,
				"loop %q has max_iterations=%d; must be >= 1",
				loop.Name, loop.MaxIterations)
		}
	}
	c.validateUnboundedLoops(w)
}

// validateUnboundedLoops enforces the "no silent infinity" invariant on
// `as <name>(unbounded)` loops: C097 (a fuel ceiling is mandatory — either the
// clause's own fuel or budget.max_iterations) and C098 (a warning when an
// unbounded loop's back-edges have no sibling `when`-exit, so only fuel/liveness
// can ever stop it).
func (c *compiler) validateUnboundedLoops(w *Workflow) {
	budgetIters := 0
	if w.Budget != nil {
		budgetIters = w.Budget.MaxIterations
	}
	for _, loop := range w.Loops {
		if !loop.Unbounded {
			continue
		}
		if loop.FuelCap <= 0 && budgetIters <= 0 {
			c.errorf(DiagUnboundedNoFuel,
				"loop %q is unbounded but has no fuel ceiling; set a per-loop fuel (as %s(unbounded <N>)) or workflow budget.max_iterations",
				loop.Name, loop.Name)
		}
		if !c.unboundedLoopHasExit(w, loop) {
			c.warnf(DiagUnboundedNoExit,
				"loop %q is unbounded and no node in its body has an edge leaving the loop; only fuel/liveness can stop it — add a `when`-exit (convergence condition)",
				loop.Name)
		}
	}
}

// unboundedLoopHasExit reports whether any node in the loop's body has an
// outgoing edge that leaves the body (an exit path) — anywhere in the cycle,
// not only at the back-edge source. Conservative: if the body is empty
// (uncomputed), assume an exit exists so we never false-warn.
func (c *compiler) unboundedLoopHasExit(w *Workflow, loop *Loop) bool {
	if len(loop.Body) == 0 {
		return true
	}
	for _, e := range w.Edges {
		if !loop.Body[e.From] || e.LoopName == loop.Name {
			continue
		}
		if !loop.Body[e.To] {
			return true // edge leaving the loop body = an exit
		}
	}
	return false
}
