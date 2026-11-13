package ir

import "strings"

// dialled is the clause a verdict owes an operator whose route is an
// environment dial. The source reading resolves `${VAR:-name}` to the NAME
// WRITTEN IN THE SOURCE — deliberately, so one artifact compiles the same on
// every machine (backendReader.resolve) — while the DISPATCH expands the same
// text from the pod's environment (ir.ExpandEnvWithDefault). The two can name
// different backends, and without this clause the operator reads a verdict
// about a backend they overrode months ago. C315 already warns that dialling
// a route is an ADR-132 §1 anti-pattern; this only makes the verdict legible.
func dialled(routeText string) string {
	if !strings.Contains(routeText, "${") {
		return ""
	}
	return " (read from the default written in the source: this route is an environment dial, and a deployment that points it elsewhere is judged at dispatch, not here)"
}

// validateCodemodeBackends refuses `codemode: on` on the routes whose lack of
// a codemode tool is knowable before launch.
//
// It is the compile half of one refusal, not a second opinion: the executor
// resolves the same value through [ResolveCodemode] and refuses the same
// backends through [CodemodeServedBy] (executor_codemode.go), so a route the
// source does not decide — empty, `auto`, a `{{vars.x}}` the launch may
// override — is checked at dispatch instead of guessed here. This is exactly
// validateAsyncBackends' division (C267), and an ERROR for its reason: what
// it diagnoses fails the dispatch, it does not degrade.
//
// The value checked is the EFFECTIVE one. A node that declares nothing
// inherits the workflow's, so a workflow-level `codemode: on` is diagnosed on
// each node that would run it — that is where the refusal will fire, and a
// diagnostic placed on the workflow line would name a node the operator then
// has to find. A node's own `codemode: off` against a workflow `on` silences
// it, which is the whole reason the attribute carries three states.
func (c *compiler) validateCodemodeBackends(w *Workflow) {
	for _, node := range w.Nodes {
		n, ok := node.(LLMNode)
		if !ok {
			continue
		}
		// ONE guard, not two. `policy.Declared()` was here as well and could
		// never fire: the only rung that answers "not declared" is the
		// default, and the default is OFF, so `!policy.On` already covers it.
		// Two guards covering each other look tested and neither is —
		// TestOnImpliesDeclared pins the invariant that makes this one
		// sufficient, and reddens the day a rung makes the default on.
		policy := ResolveCodemode(n.GetCodemode(), w.Codemode)
		if !policy.On {
			continue
		}
		primary := effectiveNodeBackend(n.GetLLMFields().Backend, w.DefaultBackend)
		// Which TEXT the clause reads, which is not the same question as
		// which BACKEND the verdict names.
		//
		// A field that falls through hands the verdict to the workflow
		// default, so the clause must read the default's text — and the
		// fall-through is asked with BackendFallsThrough, the repository's
		// one spelling of that rule, because reconstructing it as `== ""`
		// disagreed on `auto`.
		//
		// But a DIALLED field that falls through is still a dial: a
		// `backend: "${VAR:-auto}"` lands on the workflow default only when
		// nobody set VAR, and the deployment that sets it decides the route.
		// Keeping its own text is what stops the clause from vanishing
		// exactly where it is most needed. Both halves were found by
		// adversarial rounds, in that order.
		primaryText := n.GetLLMFields().Backend
		if dialled(primaryText) == "" && BackendFallsThrough(sourceBackend.routeName(primaryText)) {
			primaryText = w.DefaultBackend
		}
		check := func(backend, route, routeText string) {
			if !CodemodeRefusedBy(backend) {
				return
			}
			c.errorfAtProperty(DiagCodemodeBackendUnsupported, node.NodeKind().String(), node.NodeID(), "codemode",
				"%s %q: %s backend %q serves no codemode tool%s, and this node has %s; %s",
				node.NodeKind(), node.NodeID(), route, backend, dialled(routeText),
				policy, CodemodeRemedy(backend))
		}
		check(primary, "primary", primaryText)
		for _, fallback := range n.GetFallbacks() {
			// An omitted backend uses the primary route, already checked.
			if route := sourceBackend.routeName(fallback.Backend); route != "" && route != primary {
				check(route, "fallback "+fallbackLabel(fallback), fallback.Backend)
			}
		}
	}
}
