package runtime

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// incomingFromEdge snapshots the identity of a workflow edge so the
// runtime can remember which incoming edges actually fired into a node
// (issue #484). With-mappings from unselected siblings must not contribute.
func incomingFromEdge(edge *ir.Edge) store.IncomingEdge {
	if edge == nil {
		return store.IncomingEdge{}
	}
	return store.IncomingEdge{
		From:          edge.From,
		To:            edge.To,
		Condition:     edge.Condition,
		Negated:       edge.Negated,
		ExpressionSrc: edge.ExpressionSrc,
		IsElse:        edge.IsElse,
		LoopName:      edge.LoopName,
		ForeachName:   edge.ForeachName,
	}
}

func incomingEqual(a store.IncomingEdge, edge *ir.Edge) bool {
	if edge == nil {
		return false
	}
	return a.From == edge.From &&
		a.To == edge.To &&
		a.Condition == edge.Condition &&
		a.Negated == edge.Negated &&
		a.ExpressionSrc == edge.ExpressionSrc &&
		a.IsElse == edge.IsElse &&
		a.LoopName == edge.LoopName &&
		a.ForeachName == edge.ForeachName
}

func incomingKey(in store.IncomingEdge) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%t\x00%s\x00%t\x00%s\x00%s",
		in.From, in.To, in.Condition, in.Negated, in.ExpressionSrc, in.IsElse, in.LoopName, in.ForeachName)
}

// recordIncoming stores edge as a selected incoming edge for its destination.
// replace=true is the sequential case (this visit of dest has exactly one
// predecessor). replace=false appends (fan-out join: several branches
// independently selected an incoming edge).
func recordIncoming(dst map[string][]store.IncomingEdge, edge *ir.Edge, replace bool) {
	if dst == nil || edge == nil {
		return
	}
	ref := incomingFromEdge(edge)
	if replace {
		dst[edge.To] = []store.IncomingEdge{ref}
		return
	}
	for _, existing := range dst[edge.To] {
		if incomingEqual(existing, edge) {
			return
		}
	}
	dst[edge.To] = append(dst[edge.To], ref)
}

func (rs *runState) setIncoming(edge *ir.Edge) {
	if rs == nil {
		return
	}
	if rs.selectedIncoming == nil {
		rs.selectedIncoming = make(map[string][]store.IncomingEdge)
	}
	recordIncoming(rs.selectedIncoming, edge, true)
}

// incomingFor returns the selected incoming edges recorded for this visit
// of nodeID. tracked=false means nothing was recorded (legacy checkpoint,
// or a test that calls buildNodeInputRS without going through routing):
// the resolver then falls back to "every incoming edge whose source has
// produced output", which is the pre-#484 behaviour.
func incomingFor(nodeID string, sc resolveScope) (edges []store.IncomingEdge, tracked bool) {
	m := sc.incomingByNode
	if m == nil && sc.rs != nil {
		m = sc.rs.selectedIncoming
	}
	if m == nil {
		return nil, false
	}
	edges, ok := m[nodeID]
	return edges, ok
}

func edgeInIncoming(edge *ir.Edge, selected []store.IncomingEdge) bool {
	for i := range selected {
		if incomingEqual(selected[i], edge) {
			return true
		}
	}
	return false
}

// incomingOnlyBounded reports whether every recorded incoming edge is a
// bounded-iteration back-edge (loop or foreach). That is the loop-head
// re-entry shape: selectEdgeRS replaced the head's set with the single
// back-edge. A back-edge is an OVERLAY of the keys that change per
// iteration, not a replacement of the head's whole input — unmapped
// keys must still come from the forward/entry edges whose sources have
// output (#484 is about exclusive FORWARD siblings).
func incomingOnlyBounded(selected []store.IncomingEdge) bool {
	if len(selected) == 0 {
		return false
	}
	for i := range selected {
		if selected[i].LoopName == "" && selected[i].ForeachName == "" {
			return false
		}
	}
	return true
}

// firstEdge returns the first workflow edge from→to. Used when a router
// names a target node rather than an *ir.Edge (LLM single-mode).
func (e *Engine) firstEdge(from, to string) *ir.Edge {
	if e == nil || e.workflow == nil {
		return nil
	}
	for _, edge := range e.workflow.Edges {
		if edge.From == from && edge.To == to {
			return edge
		}
	}
	return nil
}

func cloneIncoming(m map[string][]store.IncomingEdge) map[string][]store.IncomingEdge {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string][]store.IncomingEdge, len(m))
	for k, v := range m {
		out[k] = append([]store.IncomingEdge(nil), v...)
	}
	return out
}

// mergeJoinIncoming unions the selected incoming edges that successful
// fan-out branches recorded for the convergence node, and writes that
// set onto the trunk runState so the join execution applies exactly those
// mappings.
func mergeJoinIncoming(rs *runState, joinNodeID string, results []*branchResult) {
	if rs == nil || joinNodeID == "" {
		return
	}
	var union []store.IncomingEdge
	seen := make(map[string]bool)
	for _, r := range results {
		if r == nil || r.err != nil {
			continue
		}
		for _, in := range r.selectedIncoming[joinNodeID] {
			k := incomingKey(in)
			if seen[k] {
				continue
			}
			seen[k] = true
			union = append(union, in)
		}
	}
	if len(union) == 0 {
		// No successful branch recorded an edge into the join (every
		// branch failed under best_effort, or the join came from the
		// pre-computed topology). Recording an EMPTY set marks the
		// node "tracked", which makes buildNodeInputRS drop every
		// with-mapping into it — including the ones the untracked
		// fallback would still apply.
		if rs.selectedIncoming != nil {
			delete(rs.selectedIncoming, joinNodeID)
		}
		return
	}
	if rs.selectedIncoming == nil {
		rs.selectedIncoming = make(map[string][]store.IncomingEdge)
	}
	rs.selectedIncoming[joinNodeID] = union
}
