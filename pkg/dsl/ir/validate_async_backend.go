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
