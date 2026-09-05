package ir

// FanOutInSources returns, for every node, the DISTINCT predecessors that
// count toward electing it as the collector of routerID's fan-out. Every
// non-iteration predecessor counts, with one exception: a DIRECT target of
// the router — a branch head — keeps only predecessors inside the fan-out
// region, i.e. the router itself and the nodes it reaches over
// non-iteration edges. Bounded back-edges are local control flow and never
// count.
//
// A predecessor of a branch head that lies outside the fan-out is another
// way for the trunk to reach that head without fanning out — a condition
// router that routes to the same reviewer directly (mono) or through the
// fan-out (dual). It is no evidence that the branches reconverge there, and
// counting it elects the fan-out's own target as its collector: that
// target's branch stops before executing anything, its sibling runs the
// whole post-fan-out chain inside its branch, and the trunk then runs the
// same chain a second time.
//
// A predecessor from outside the fan-out that reaches a node BELOW the head
// is a trunk bypass into the region (`plan -> collect else`, the no-items
// case): that node collects the branches and the bypass alike, and the
// bypass edge is what makes it the implicit collector of a linear template
// that declares no `await:`. It keeps counting.
func FanOutInSources(w *Workflow, routerID string) map[string]map[string]bool {
	region := map[string]bool{routerID: true}
	targets := make(map[string]bool)
	queue := []string{routerID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, edge := range w.Edges {
			if edge == nil || edge.From != id || edge.IsBoundedIteration() {
				continue
			}
			if id == routerID {
				targets[edge.To] = true
			}
			if region[edge.To] {
				continue
			}
			region[edge.To] = true
			queue = append(queue, edge.To)
		}
	}
	in := make(map[string]map[string]bool)
	for _, edge := range w.Edges {
		if edge == nil || edge.IsBoundedIteration() {
			continue
		}
		if targets[edge.To] && !region[edge.From] {
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
// or has more than one distinct counted predecessor (FanOutInSources); ""
// means the branches never reconverge, each ending at its own terminal. The
// runtime and the compile-time ownership checks share this election so that
// C243/C244 stop at the node the engine stops at.
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
