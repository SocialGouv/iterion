package ir

// FanOutInSources returns, for every node, its DISTINCT predecessors that
// belong to the fan-out region of routerID: the router itself and every node
// reachable from it over non-iteration edges. Bounded back-edges are local
// control flow and contribute neither to the region nor to a node's sources.
//
// A predecessor outside the region is a path that reaches the node without
// going through the fan-out at all — a condition router that routes to the
// same reviewer directly (mono) or through the fan-out (dual) — so it is no
// evidence that the fan-out's branches reconverge there. Counting it elects
// the fan-out's own target as its collector: that target's branch stops
// before executing anything, its sibling runs the whole post-fan-out chain
// inside its branch, and the trunk then runs the same chain a second time.
func FanOutInSources(w *Workflow, routerID string) map[string]map[string]bool {
	region := map[string]bool{routerID: true}
	queue := []string{routerID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, edge := range w.Edges {
			if edge == nil || edge.From != id || edge.IsBoundedIteration() || region[edge.To] {
				continue
			}
			region[edge.To] = true
			queue = append(queue, edge.To)
		}
	}
	in := make(map[string]map[string]bool)
	for _, edge := range w.Edges {
		if edge == nil || edge.IsBoundedIteration() || !region[edge.From] {
			continue
		}
		if in[edge.To] == nil {
			in[edge.To] = make(map[string]bool)
		}
		in[edge.To][edge.From] = true
	}
	return in
}

// ExecBranchConvergencePoint elects the node at which the branches spawned by
// routerID's fanEdges reconverge: the collector every branch stops at, which
// the trunk then executes ONCE after all of them have settled. A breadth-first
// walk from the ordered targets elects the first node that declares `await:`
// or has more than one distinct predecessor inside the fan-out region
// (FanOutInSources); "" means the branches never reconverge, each ending at
// its own terminal. The runtime and the compile-time ownership checks share
// this election so that C243/C244 stop at the node the engine stops at.
func ExecBranchConvergencePoint(w *Workflow, routerID string, fanEdges []*Edge) string {
	inSources := FanOutInSources(w, routerID)
	for _, startEdge := range fanEdges {
		visited := make(map[string]bool)
		queue := []string{startEdge.To}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			if visited[id] {
				continue
			}
			visited[id] = true
			node := w.Nodes[id]
			if node == nil {
				continue
			}
			if NodeAwaitMode(node) != AwaitNone || len(inSources[id]) > 1 {
				return id
			}
			for _, edge := range w.Edges {
				if edge.From == id {
					queue = append(queue, edge.To)
				}
			}
		}
	}
	return ""
}
