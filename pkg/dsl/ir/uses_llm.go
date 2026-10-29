package ir

// This file answers two different questions about model spend, and the
// distinction between them is the whole point.
//
//   UsesLLM          — "does this workflow contain a node that can call a
//                      model?" A property of the graph.
//   AlwaysReachesLLM — "will EVERY run of this workflow reach one?" A
//                      property of every path through the graph.
//
// A guard that refuses work BEFORE it starts — the operator's subscription
// cap — must ask the second. The first is not enough, and the gap between
// them is not academic: a two-mode bot carries both halves in one `.bot`,
// and refusing its zero-LLM half because the LLM half exists in the same
// file is exactly the defect this pair was written to close.

// UsesLLM reports whether the workflow contains at least one node that can
// call a model, anywhere in the graph, reachable or not.
//
// Deliberately CONSERVATIVE: every uncertainty answers true. A subbot
// counts because its child `.bot` is a separate source this workflow does
// not carry; a supervisor counts even when no graph node does, because it
// watches with a model of its own.
func (w *Workflow) UsesLLM() bool {
	if w == nil {
		return false
	}
	if len(w.Supervisors) > 0 {
		return true
	}
	for _, n := range w.Nodes {
		if nodeUsesLLM(n) {
			return true
		}
	}
	return false
}

// AlwaysReachesLLM reports whether EVERY path from the entry node to a
// terminal passes through a node that can call a model — i.e. whether this
// workflow is incapable of running without spending.
//
// It is the predicate a pre-flight guard needs. Refusing a run in advance
// is only defensible when the run could not possibly avoid the thing being
// guarded; when some path avoids it, the honest answer is to let the run
// start and let the MID-RUN guard stop it at the actual call. That costs a
// pod and a clone in the worst case, and it is the price of not refusing
// work that would never have been billed.
//
// The routing that decides which path a run takes is usually not knowable
// here — Vigie's `plan -> fetch_feeds when collect` branches on a field the
// `plan` node produces at runtime, not on a var — so this deliberately does
// not try to predict it. It asks the weaker, decidable question: does a
// model-free path exist at all?
//
// ONE exception, and only for a caller that NARROWS a refusal: a run's launch
// vars are fixed for its whole life, so an edge whose `when` reads nothing but
// those vars has the same answer here as at the crossing
// (LaunchVarEdgeFilter, passed to AlwaysReachesVia / CanReachVia). This
// predicate takes no filter — it decides whether a cap applies at all, so
// narrowing it would REFUSE runs a model-free branch excludes rather than
// admit them, which is the opposite of what the exception is sound for.
//
// Conservative in the direction that matters: an empty or unreachable
// graph, a workflow whose entry is missing, or any shape this cannot walk
// answers true, keeping today's behaviour rather than opening the gate.
func (w *Workflow) AlwaysReachesLLM() bool {
	if w == nil || len(w.Nodes) == 0 {
		return true
	}
	// A supervisor is armed for the whole run whatever path it takes, so
	// no path avoids the spend.
	if len(w.Supervisors) > 0 {
		return true
	}
	return w.AlwaysReaches(nodeUsesLLM)
}

// AlwaysReaches reports whether EVERY path from the entry node to a terminal
// passes through a node wall accepts — the question AlwaysReachesLLM asks,
// over any wall. A pre-flight that refuses a run in advance asks it with the
// walls it guards: the run cannot avoid them only when no execution reaches a
// terminal around every one of them.
//
// A node picks ONE outgoing edge, and so does a condition, round-robin or llm
// router: one free successor frees it. A fan-out router (fan_out_all,
// fan_out_each) runs its branches, all of them: it is free only when every
// branch is. The answer is a least fixpoint, so it does not depend on the
// order nodes or edges are read in, and a cycle that reaches no terminal
// frees nothing.
//
// Conservative like AlwaysReachesLLM: an empty graph, a missing entry, an
// edge from a reachable node into a node the workflow does not define answer
// true. Supervisors are not graph nodes and are not consulted: a caller whose
// walls include them says so itself.
func (w *Workflow) AlwaysReaches(wall func(Node) bool) bool {
	return w.AlwaysReachesVia(nil, wall)
}

// EdgeFilter reports whether a walk may follow an edge. A walk given one
// answers over the executions those edges allow, rather than over every
// execution the graph admits; nil follows every edge.
type EdgeFilter func(*Edge) bool

// AlwaysReachesVia is AlwaysReaches over the executions open allows — the
// question a pre-flight asks about ONE run whose launch already excluded some
// of the graph's transitions (LaunchVarEdgeFilter).
//
// It narrows in the direction that REFUSES: with fewer transitions, fewer
// paths reach a terminal around the walls, so a run whose remaining paths all
// cross one is refused in advance instead of being stopped at the call. That
// is sound only for a filter whose dropped edges the run could not have taken.
// A node whose exits the filter excludes has its own end (walkNode.stopped),
// which is free unless the node itself is a wall. A bounded iteration ceases
// to be a crossing when spent; its retained edge cannot erase that end.
func (w *Workflow) AlwaysReachesVia(open EdgeFilter, wall func(Node) bool) bool {
	out, ok := w.walkable(open)
	if !ok {
		return true
	}
	walled := make(map[string]bool, len(w.Nodes))
	for id, n := range w.Nodes {
		walled[id] = wall(n)
	}
	free := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for id, n := range w.Nodes {
			if free[id] || walled[id] {
				continue
			}
			next := out[id].successors()
			var isFree bool
			switch {
			case isTerminal(n), out[id].stopped:
				isFree = true
			case runsEveryBranch(n):
				isFree = len(next) > 0
				for _, to := range next {
					if !free[to] {
						isFree = false
						break
					}
				}
			default:
				for _, to := range next {
					if free[to] {
						isFree = true
						break
					}
				}
			}
			if isFree {
				free[id] = true
				changed = true
			}
		}
	}
	return !free[w.Entry]
}

// CanReach reports whether some execution from the entry node reaches a node
// target accepts. Conservative: a graph walkable() refuses answers true.
func (w *Workflow) CanReach(target func(Node) bool) bool {
	return w.CanReachVia(nil, target)
}

// CanReachVia is CanReach over the executions open allows: a target only the
// dropped transitions led to is not one this run can reach. It consumes the
// same walkNode annotation as AlwaysReachesVia: the dying node still runs,
// and its iteration body may run before exhaustion, but no excluded exit does.
func (w *Workflow) CanReachVia(open EdgeFilter, target func(Node) bool) bool {
	out, ok := w.walkable(open)
	if !ok {
		return true
	}
	seen := map[string]bool{w.Entry: true}
	stack := []string{w.Entry}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if target(w.Nodes[id]) {
			return true
		}
		for _, to := range out[id].successors() {
			if !seen[to] {
				seen[to] = true
				stack = append(stack, to)
			}
		}
	}
	return false
}

// walkNode describes both crossings and an end caused by the node itself.
// stopped means the filter excludes every exit: once its bounded iterations
// are spent, the run dies here rather than crossing a wall. Iteration work
// remains reachable while it has budget.
type walkNode struct {
	next       []string
	iterations []string
	stopped    bool
}

func (n walkNode) successors() []string {
	if n.stopped {
		return n.iterations
	}
	return n.next
}

// walkable returns the graph's annotated successor lists, or false when a
// walk from the entry cannot conclude anything: no nodes, no entry, or an
// edge from a node the entry reaches into a node the workflow does not define.
//
// open, when given, keeps only the edges it accepts — except an edge into a
// node the workflow does not define, which stays in its source's successor
// list whatever the filter says: a graph whose REACHABLE part still holds one
// stays refused, so no predicate can lift the refusal by claiming the
// malformed edge itself. A filter that puts the SOURCE off the run's paths
// does conclude, on the run the filter describes: the malformed node is one
// it never reaches.
func (w *Workflow) walkable(open EdgeFilter) (map[string]walkNode, bool) {
	if w == nil || len(w.Nodes) == 0 || w.Entry == "" || w.Nodes[w.Entry] == nil {
		return nil, false
	}
	out := map[string]walkNode{}
	excluded := map[string]bool{}
	keptExit := map[string]bool{}
	for _, e := range w.Edges {
		if e == nil {
			continue
		}
		if open != nil && w.Nodes[e.To] != nil && !open(e) {
			excluded[e.From] = true
			continue
		}
		n := out[e.From]
		n.next = append(n.next, e.To)
		if e.IsBoundedIteration() {
			n.iterations = append(n.iterations, e.To)
		} else {
			keptExit[e.From] = true
		}
		out[e.From] = n
	}
	// This is the single classification shared by both walks. An unknown
	// guard or a malformed target keeps an exit, and an unfiltered dead end
	// stays conservative. A bounded back-edge is not an exit after exhaustion.
	for id := range excluded {
		n := out[id]
		n.stopped = !keptExit[id]
		out[id] = n
	}
	seen := map[string]bool{w.Entry: true}
	stack := []string{w.Entry}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, to := range out[id].successors() {
			if w.Nodes[to] == nil {
				return nil, false
			}
			if !seen[to] {
				seen[to] = true
				stack = append(stack, to)
			}
		}
	}
	return out, true
}

func isTerminal(n Node) bool {
	switch n.(type) {
	case *DoneNode, *FailNode:
		return true
	}
	return false
}

// runsEveryBranch reports whether a node executes ALL its outgoing edges
// rather than choosing one.
func runsEveryBranch(n Node) bool {
	r, ok := n.(*RouterNode)
	return ok && (r.RouterMode == RouterFanOutAll || r.RouterMode == RouterFanOutEach)
}

// NodeUsesLLM reports whether one node can call a model — the per-node
// half of UsesLLM, exported for the walks outside this package that must
// stay CONSERVATIVE about spend (a credential-wants derivation that
// narrows a pool request, a usage-cap wire predicate): a node this answers
// true for but that exposes no LLMFields (a subbot) cannot be resolved to
// a provider by shape alone, and a caller must widen rather than assume it
// spends nothing. The two model-calling kinds that carry no LLMFields but
// DO expose their route — a human node's LLM half and a Verified Action's
// recovery rungs — are read through their own shared readers
// (model.humanLLMModelOf, model.recoveryRungsOf).
func NodeUsesLLM(n Node) bool { return nodeUsesLLM(n) }

// ToolRecoveryAgentRung reports whether a tool node's Verified Action
// recovery can reach rung 4 — the AGENT dispatch, which carries a tool set
// (`agent_tools:`) and is gated by the workflow's permission lists. Rung 3
// (self-repair) is a direct generation with no tool surface, so it is
// deliberately NOT this predicate's answer: the permission validation's
// "reader" is the rung that holds tools, not the one that only completes.
//
// Dispatch's own two conditions, and the companion of
// ToolRecoverySelfRepairRung: the rungs exist under `policy: recover` alone,
// so an inert `recovery:` block (C106) is no reader of the permission lists
// — nothing can ever run to read them. Unlike rung 3 this one takes the
// recipe as it finds it: an agent achieves the GOAL with real tools, so it
// runs for a registry recipe the self-repair rung cannot correct.
func ToolRecoveryAgentRung(n Node) bool {
	t, ok := n.(*ToolNode)
	return ok && t.Policy == PolicyRecover && t.Recovery != nil &&
		t.Recovery.MaxAgentAttempts > 0
}

// nodeUsesLLM answers for one node. Kept separate so the reasoning per node
// kind stays readable, and each `true` names why it is one.
func nodeUsesLLM(n Node) bool {
	switch node := n.(type) {
	case *AgentNode, *JudgeNode:
		// The two node kinds whose whole purpose is a model call.
		return true
	case *RouterNode:
		// Only the `llm` routing mode asks a model where to go; the
		// deterministic modes (fan_out_*, condition, round_robin) do not.
		return node.RouterMode == RouterLLM
	case *HumanNode:
		// `interaction: llm` answers the question with a model instead of
		// a human, and `llm_or_human` tries that first.
		return interactionUsesLLM(node.Interaction)
	case *ToolNode:
		// A tool node is a shell command — except that a Verified Action's
		// recovery ladder spends a model when the recipe cannot heal
		// itself: rung 3 asks one for a corrected command (a direct
		// generation), rung 4 hands recovery to an agent. A run that only
		// *might* reach a rung still can. Both rungs are read through the
		// predicates dispatch gates them on, never restated here: a rung
		// this arm counts and dispatch never runs makes a launch refuse a
		// provider nothing spends (#2783).
		return ToolRecoverySelfRepairRung(node) || ToolRecoveryAgentRung(node)
	case *SubbotNode:
		// The child `.bot` is another source entirely: unknowable from
		// here, so assumed to spend.
		return true
	}
	return false
}

// interactionUsesLLM reports whether an interaction mode can answer with a
// model rather than by parking for a human.
func interactionUsesLLM(m InteractionMode) bool {
	return m == InteractionLLM || m == InteractionLLMOrHuman
}
