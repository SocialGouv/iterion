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
	out, ok := w.walkable()
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
			next := out[id]
			var isFree bool
			switch {
			case isTerminal(n):
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
	out, ok := w.walkable()
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
		for _, to := range out[id] {
			if !seen[to] {
				seen[to] = true
				stack = append(stack, to)
			}
		}
	}
	return false
}

// walkable returns the graph's successor lists, or false when a walk from the
// entry cannot conclude anything: no nodes, no entry, or an edge from a node
// the entry reaches into a node the workflow does not define.
func (w *Workflow) walkable() (map[string][]string, bool) {
	if w == nil || len(w.Nodes) == 0 || w.Entry == "" || w.Nodes[w.Entry] == nil {
		return nil, false
	}
	out := map[string][]string{}
	for _, e := range w.Edges {
		if e != nil {
			out[e.From] = append(out[e.From], e.To)
		}
	}
	seen := map[string]bool{w.Entry: true}
	stack := []string{w.Entry}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, to := range out[id] {
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
// true for but that exposes no LLMFields (a human node answering with a
// model, a Verified Action's agent rung, a subbot) cannot be resolved to
// a provider, and a caller must widen rather than assume it spends nothing.
func NodeUsesLLM(n Node) bool { return nodeUsesLLM(n) }

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
		// rung 4 hands recovery to an agent when the recipe cannot heal
		// itself. A run that only *might* reach that rung still can.
		return node.Recovery != nil && node.Recovery.MaxAgentAttempts > 0
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
