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
	entry := w.Entry
	if entry == "" || w.Nodes[entry] == nil {
		return true
	}

	// Walk forward from the entry, treating an LLM node as a WALL: paths
	// through it spend, so they are not explored. Reaching a terminal
	// without hitting a wall proves a model-free path exists.
	out := map[string][]string{}
	for _, e := range w.Edges {
		if e != nil {
			out[e.From] = append(out[e.From], e.To)
		}
	}
	seen := map[string]bool{entry: true}
	stack := []string{entry}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		node := w.Nodes[id]
		if node == nil {
			// An edge into a node the workflow does not define: unwalkable,
			// so refuse to conclude "a free path exists" from it.
			return true
		}
		if nodeUsesLLM(node) {
			continue // wall
		}
		switch node.(type) {
		case *DoneNode, *FailNode:
			return false // a terminal reached without spending
		}
		next := out[id]
		if len(next) == 0 {
			// A dead end that is not a terminal — the runtime would fail
			// here rather than finish. Not a proof of a free path.
			continue
		}
		for _, to := range next {
			if !seen[to] {
				seen[to] = true
				stack = append(stack, to)
			}
		}
	}
	return true
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
