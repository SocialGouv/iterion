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
// incomingMatchesWorkflow reports whether at least one recorded incoming
// edge still exists on the current workflow. A resume --force / rewind
// against an edited .bot can rehydrate identities that no longer match
// (renamed source, changed `when`, flipped `else`); treating that set as
// tracked would drop every with-mapping (R25212d).
func incomingMatchesWorkflow(nodeID string, selected []store.IncomingEdge, edges []*ir.Edge) bool {
	for _, edge := range edges {
		if edge != nil && edge.To == nodeID && edgeInIncoming(edge, selected) {
			return true
		}
	}
	return false
}

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
// mappings. launched is the set of edges this invocation actually started
// branches on — the provenance of the settled floor recorded alongside.
func (e *Engine) mergeJoinIncoming(rs *runState, joinNodeID string, results []*branchResult, launched []*ir.Edge) {
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
	// Record what this invocation settled on, whichever way it went: a
	// fresh invocation owns the floor for its join, so one that produced
	// output must not leave the previous one's floor standing.
	rs.setSettledFloor(joinNodeID, settledEdgesInto(e.workflow, launched, joinNodeID))
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

// ---------------------------------------------------------------------------
// Settled floor — the edges a stabilized fan-out left with no output behind
// ---------------------------------------------------------------------------

// settledEdgesInto returns the edges into joinNodeID that a fan-out
// invocation would have fired, found by walking forward from the nodes it
// actually entered.
//
// Provenance is the LAUNCHED edges, never the declared ones: an llm router
// in multi-select mode starts a subset of what the graph declares, so
// reading the declaration would sweep in the very foreign edge the floor
// exists to exclude.
//
// Bounded-iteration edges are out of both the walk and the result. A
// back-edge is a per-iteration overlay applied last, not part of a
// stabilized forward pass (Rae4900).
func settledEdgesInto(wf *ir.Workflow, launched []*ir.Edge, joinNodeID string) []store.IncomingEdge {
	if wf == nil || joinNodeID == "" || len(launched) == 0 {
		return nil
	}
	reachable := make(map[string]bool, len(launched))
	frontier := make([]string, 0, len(launched))
	push := func(id string) {
		if id == "" || reachable[id] {
			return
		}
		reachable[id] = true
		frontier = append(frontier, id)
	}
	for _, edge := range launched {
		if edge == nil || edge.IsBoundedIteration() {
			continue
		}
		push(edge.To)
	}
	// Forward closure, stopping AT the join: expanding past it could
	// re-enter through an unrelated downstream cycle and claim edges this
	// invocation never owned.
	for len(frontier) > 0 {
		node := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		if node == joinNodeID {
			continue
		}
		for _, edge := range wf.Edges {
			if edge == nil || edge.From != node || edge.IsBoundedIteration() {
				continue
			}
			push(edge.To)
		}
	}
	var settled []store.IncomingEdge
	for _, edge := range wf.Edges {
		if edge == nil || edge.To != joinNodeID || edge.IsBoundedIteration() {
			continue
		}
		if edge.From == "" || !reachable[edge.From] {
			continue
		}
		settled = append(settled, incomingFromEdge(edge))
	}
	return settled
}

// setSettledFloor stores — or clears — the settled floor for a convergence
// node. Kept apart from selectedIncoming because a join may be a loop head
// (TestValidateLoopAfterJoin_Allowed): selectEdgeRS REPLACES the selection
// on re-entry, so a floor living there would vanish on the second visit.
func (rs *runState) setSettledFloor(joinNodeID string, settled []store.IncomingEdge) {
	if rs == nil || joinNodeID == "" {
		return
	}
	if len(settled) == 0 {
		delete(rs.settledIncoming, joinNodeID)
		return
	}
	if rs.settledIncoming == nil {
		rs.settledIncoming = make(map[string][]store.IncomingEdge)
	}
	rs.settledIncoming[joinNodeID] = settled
}

// settledFloorFor returns the floor recorded for nodeID, if any.
func settledFloorFor(nodeID string, sc resolveScope) []store.IncomingEdge {
	if sc.rs == nil {
		return nil
	}
	return sc.rs.settledIncoming[nodeID]
}

// settledFloorEligible reports whether edge draws its with-mappings from
// the floor recorded for its destination. THE shared eligibility rule:
// buildNodeInputRS and consumedArtifactRefs both consult it, so an edge
// that feeds the join can never sit outside the join's artifact contract.
//
// Only an output-less source qualifies. An edge whose source ran is left
// to the ordinary passes, where routing's recorded selection still decides
// between exclusive siblings — without that clause a `when`/`else` pair
// downstream of a live node would both contribute again (#484).
func settledFloorEligible(edge *ir.Edge, floor []store.IncomingEdge, hasOutput bool) bool {
	if edge == nil || len(floor) == 0 || hasOutput || edge.From == "" {
		return false
	}
	if edge.IsBoundedIteration() {
		return false
	}
	return edgeInIncoming(edge, floor)
}
