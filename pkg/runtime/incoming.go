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
	if edge != nil && !edge.IsBoundedIteration() {
		// A FORWARD arrival: routing picked a path into this node that is not
		// the fan-out that settled its floor, so that floor belongs to an
		// earlier visit. Keeping it would feed the node with-mappings from
		// edges this visit's routing did not select, through a path that
		// bypasses the tracked/selected filter — the #484 shape. This is the
		// ONE place a forward selection replaces a node's incoming set; the
		// fan-out writes the join's selection directly (mergeJoinIncoming),
		// so the fed visit and its resume are untouched.
		//
		// A bounded-iteration edge is excluded deliberately: a loop head
		// re-entered by its own back-edge is the SAME visit continuing, and
		// its dead branches are still dead — dropping the floor there would
		// make their mappings vanish from iteration 2 on.
		delete(rs.settledIncoming, edge.To)
	}
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
// mappings. seeds are the nodes this invocation actually entered — the
// provenance of the settled floor recorded alongside.
func (e *Engine) mergeJoinIncoming(rs *runState, joinNodeID string, results []*branchResult, seeds []string) {
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
	rs.setSettledFloor(joinNodeID, settledEdgesInto(e.workflow, seeds, joinNodeID, evidenceFromBranches(results)))
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

// invocationEvidence is what a fan-out invocation actually DID: the nodes it
// executed to completion, and the edges that fired into each node it entered.
//
// It is read from the branch results, the FAILED ones included, because the
// trunk is not a witness of this: processConvergence merges only successful
// branches' outputs and never removes what an earlier invocation left behind,
// so "absent from rs.outputs" conflates "never ran", "ran and failed" and
// "ran two invocations ago". Anchoring the floor on the trunk made the walk
// resurrect a route routing had rejected, and made a partial failure lose the
// mapping it was supposed to keep.
type invocationEvidence struct {
	// ran holds the nodes that produced output IN THIS INVOCATION.
	ran map[string]bool
	// chosen holds, per destination, the edges that actually fired into it.
	chosen map[string][]store.IncomingEdge
}

// evidenceFromBranches gathers what the branches of one invocation recorded.
// An invocation that launched nothing (an empty `fan_out_each`) yields empty
// evidence, which is the truthful answer: no node ran, no route was rejected,
// and the caller's declared edges are then the only provenance available.
func evidenceFromBranches(results []*branchResult) invocationEvidence {
	ev := invocationEvidence{ran: map[string]bool{}, chosen: map[string][]store.IncomingEdge{}}
	for _, r := range results {
		if r == nil {
			continue
		}
		for nodeID := range r.outputs {
			ev.ran[nodeID] = true
		}
		for dst, edges := range r.selectedIncoming {
			ev.chosen[dst] = append(ev.chosen[dst], edges...)
		}
	}
	return ev
}

// settledSeedsPerEdge returns the node the walk starts from FOR EACH launched
// edge: the start node the branch that edge launched actually reported, or
// the edge's own target when that branch never started.
//
// Per edge, never per invocation. A `fan_out_all` gives every branch its own
// target, so one sibling reporting a start node says nothing about a sibling
// that bailed before execBranch — a slot acquired on an already-cancelled
// fan-out, a resume-turn error, a panic caught before the start all yield a
// result with an empty start node. Reading the invocation as a whole dropped
// that branch's subgraph from the floor entirely, which is the very mapping
// loss this exists to prevent.
func settledSeedsPerEdge(routerNodeID string, launched []*ir.Edge, results []*branchResult) []string {
	started := make(map[string]string, len(results))
	for _, r := range results {
		if r != nil && r.startNodeID != "" {
			started[r.branchID] = r.startNodeID
		}
	}
	var seeds []string
	seen := map[string]bool{}
	push := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			seeds = append(seeds, id)
		}
	}
	for _, edge := range launched {
		if edge == nil || edge.IsBoundedIteration() {
			continue
		}
		if start, ok := started[fmt.Sprintf("branch_%s_%s", routerNodeID, edge.To)]; ok {
			push(start)
			continue
		}
		push(edge.To)
	}
	return seeds
}

// settledSeedsForTemplate is the `fan_out_each` shape, where every item
// replays the SAME template subgraph: one branch's recorded start node speaks
// for all of them, and it OUTRANKS the template edge because a resumed cursor
// names the node the branch is really running even when an edit re-pointed
// the template. Only an invocation where NO branch started at all falls back
// on the declaration.
func settledSeedsForTemplate(tmplEdge *ir.Edge, results []*branchResult) []string {
	var seeds []string
	seen := map[string]bool{}
	for _, r := range results {
		if r != nil && r.startNodeID != "" && !seen[r.startNodeID] {
			seen[r.startNodeID] = true
			seeds = append(seeds, r.startNodeID)
		}
	}
	if len(seeds) > 0 {
		return seeds
	}
	if tmplEdge != nil && !tmplEdge.IsBoundedIteration() {
		return []string{tmplEdge.To}
	}
	return nil
}

// settledEdgesInto returns the edges into joinNodeID that a fan-out
// invocation would have fired, found by walking forward from the nodes it
// actually entered.
//
// seeds are the nodes this invocation actually entered, computed by the
// caller because the two fan-out shapes differ: `fan_out_all` gives each
// branch its own target (settledSeedsPerEdge), `fan_out_each` replays one
// template for every item (settledSeedsForTemplate). They come from the
// LAUNCHED edges, never the declared ones — an llm router in multi-select
// mode starts a subset of what the graph declares, so reading the declaration
// would sweep in the very foreign edge the floor exists to exclude.
//
// The walk is ROUTING-AWARE, which is what keeps it from resurrecting a
// rejected route: leaving a node the invocation executed, it follows only the
// edge that node's branch recorded as firing. Leaving a node that did NOT run
// it follows every forward edge — that is the whole point, since the node
// that would have carried the mapping is precisely the one that died.
//
// Bounded-iteration edges are out of both the walk and the result. A
// back-edge is a per-iteration overlay applied last, not part of a
// stabilized forward pass (Rae4900).
func settledEdgesInto(wf *ir.Workflow, seeds []string, joinNodeID string, ev invocationEvidence) []store.IncomingEdge {
	if wf == nil || joinNodeID == "" || len(seeds) == 0 {
		return nil
	}
	reachable := make(map[string]bool, len(seeds))
	frontier := make([]string, 0, len(seeds))
	push := func(id string) {
		if id == "" || reachable[id] {
			return
		}
		reachable[id] = true
		frontier = append(frontier, id)
	}
	for _, id := range seeds {
		push(id)
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
			if ev.ran[node] && !edgeInIncoming(edge, ev.chosen[edge.To]) {
				// This node ran and routing did not take this edge.
				// Walking it anyway is how an unselected `else` used to
				// reach the join through a node that never executed (#484).
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
		if ev.ran[edge.From] {
			// The source executed: its edges are live, and which of them
			// contributes is routing's recorded selection to apply, not the
			// floor's to guess.
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
// Membership is the whole rule. What keeps an edge out of a floor lives at
// the moment it is SETTLED (settledEdgesInto): a source that ran contributes
// through routing's recorded selection instead, and a route routing rejected
// is never walked. Re-deciding that here against the trunk's outputs is what
// made a partial failure lose its mapping — a stale output from an earlier
// invocation reads exactly like a live one.
func settledFloorEligible(edge *ir.Edge, floor []store.IncomingEdge) bool {
	if edge == nil || len(floor) == 0 || edge.From == "" {
		return false
	}
	if edge.IsBoundedIteration() {
		return false
	}
	return edgeInIncoming(edge, floor)
}
