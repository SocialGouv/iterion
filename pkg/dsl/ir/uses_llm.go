package ir

// UsesLLM reports whether executing this workflow can result in at least
// one model call.
//
// It exists for the guards that protect a *model* budget — the operator's
// subscription cap first among them. Such a guard refusing to start a
// workflow that would never call a model protects nothing and costs
// everything: the zero-LLM half of a two-mode bot (a feed collector that
// only polls RSS and appends to a queue) stops running while the window is
// shut, and the material it was supposed to bank is simply lost, because a
// feed serves a short window and does not remember what nobody fetched.
//
// The answer is deliberately CONSERVATIVE: every uncertainty returns true,
// so a guard keeps blocking exactly what it blocks today and this predicate
// can only ever open the door for a workflow provably free of model calls.
// A subbot is the clearest case — its child `.bot` is a separate source
// this workflow does not carry, so it counts as LLM-using on principle.
func (w *Workflow) UsesLLM() bool {
	if w == nil {
		return false
	}
	// A supervisor is an LLM agent watching the run from the side; it can
	// wake and spend even when no graph node ever would.
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
