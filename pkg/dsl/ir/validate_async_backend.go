package ir

// validateAsyncBackends diagnoses the built-in routes whose lack of async
// tools is knowable before launch. A route the source does not decide —
// empty, `auto`, a `{{vars.x}}` the launch may override — is checked
// against its actual capability by the executor at dispatch.
func (c *compiler) validateAsyncBackends(w *Workflow) {
	for _, node := range w.Nodes {
		n, ok := node.(LLMNode)
		if !ok || n.GetInteractionFields().Interaction != InteractionAsync {
			continue
		}
		primary := effectiveNodeBackend(n.GetLLMFields().Backend, w.DefaultBackend)
		check := func(backend, route string) {
			switch backend {
			case "codex", "kimi", "grok", "opencode":
				c.errorfAt(DiagAsyncBackendUnsupported, node.NodeID(), "",
					"%s %q: %s backend %q cannot serve interaction: async — ask_user_async and await_answers are unavailable", node.NodeKind(), node.NodeID(), route, backend)
			}
		}
		check(primary, "primary")
		for _, fallback := range n.GetFallbacks() {
			// An omitted backend uses the primary route, already checked.
			if route := sourceBackend.routeName(fallback.Backend); route != "" && route != primary {
				check(route, "fallback "+fallbackLabel(fallback))
			}
		}
	}
}

// validateSyncInteractionBackends is the SYNC half of the interaction
// capability screen (#1644). C267 above refuses interaction: async on the
// backends whose question tools do not exist there; the synchronous form
// was silence — an agent or judge declaring interaction: human (or llm /
// llm_or_human / human_or_host) on codex, kimi, grok or opencode gets no
// ask_user at all. The runtime grants the tool only to claw outright, or
// to a node whose tools: list constrains the route
// (assembleEffectiveTools) — and on those four backends the list never
// reaches the CLI argv (toolcatalog.ReceivesToolList), while the ask_user
// wiring itself exists only for claude_code (a native MCP server), claw
// (the in-process registry) and pi (the embedded RPC extension). What the
// four keep is the PROMPT text of the interaction protocol
// (BuildSystemPrompt appends it wherever InteractionEnabled), so the node
// runs to completion without ever pausing.
//
// A warning, not C267's error: the async pair is type-asserted at
// dispatch and the run fails without it, while the sync form degrades to
// prompt text — the ticket's requirement is "not silence", and a fielded
// bot that tolerates the degradation should not break at upgrade.
func (c *compiler) validateSyncInteractionBackends(w *Workflow) {
	for _, node := range w.Nodes {
		n, ok := node.(LLMNode)
		if !ok {
			continue
		}
		interaction := n.GetInteractionFields().Interaction
		if interaction == InteractionNone || interaction == InteractionAsync {
			continue
		}
		primary := effectiveNodeBackend(n.GetLLMFields().Backend, w.DefaultBackend)
		check := func(backend, route string) {
			switch backend {
			case "codex", "kimi", "grok", "opencode":
				c.warnfAt(DiagSyncInteractionInert, node.NodeID(), "",
					"%s %q: %s backend %q cannot serve interaction: %s — no ask_user tool reaches the agent there, so the node runs to completion without ever pausing (the interaction protocol is prompt text only on that backend)",
					node.NodeKind(), node.NodeID(), route, backend, interaction)
			}
		}
		check(primary, "primary")
		for _, fallback := range n.GetFallbacks() {
			// An omitted backend uses the primary route, already checked.
			if route := sourceBackend.routeName(fallback.Backend); route != "" && route != primary {
				check(route, "fallback "+fallbackLabel(fallback))
			}
		}
	}
}
