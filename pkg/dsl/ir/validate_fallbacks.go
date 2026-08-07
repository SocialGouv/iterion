package ir

import (
	"fmt"
	"strings"
)

// `fallbacks:` diagnostics (ADR-087).
const (
	DiagFallbackMalformed   DiagCode = "C173" // a fallback route declares nothing routable, or changes backend without a model (error)
	DiagFallbackUnknownOn   DiagCode = "C175" // unknown `on:` trigger token (warning)
	DiagFallbackUnsafeCross DiagCode = "C176" // a capability crossing that cannot degrade safely (error)
	DiagFallbackDrift       DiagCode = "C177" // a capability that silently changes across the chain (warning)
)

// KnownFallbackTriggers is the soft set of `on:` tokens. It mirrors
// delegate.FallbackCategory, deliberately duplicated as literals rather
// than imported: pkg/dsl/ir must not depend on pkg/backend/delegate (the
// package layout forbids it — see the codexBackendName comment in
// compile.go). Unknown tokens WARN rather than fail so an out-of-tree
// runtime can extend the vocabulary.
//
// `unclassified` is absent on purpose: it is not a condition an author
// can name, it is the absence of one. The runtime always routes on it
// (refusing would strand a run on exactly the failures iterion could not
// describe), which makes naming it meaningless.
var KnownFallbackTriggers = map[string]bool{
	"usage_window":        true,
	"auth":                true,
	"unavailable":         true,
	"transient_exhausted": true,
	"any":                 true,
}

// gateEnforcingBackends are the backends that can enforce iterion's
// tool-permission gate. A `fallbacks:` route on any other backend would
// run the node UNGATED — the anti-prompt-injection boundary silently
// disappearing at the moment the run is under stress. pi already refuses
// rather than degrades in this situation; C176 adopts that precedent at
// compile time.
var gateEnforcingBackends = map[string]bool{
	"claude_code": true,
	"claw":        true,
	"pi":          true,
}

// reasoningEffortBackends are the backends that carry a
// reasoning-effort dial. Deliberately its OWN set rather than a reuse
// of gateEnforcingBackends: that set answers "can this backend enforce
// the permission gate", and the two memberships differ — grok passes
// `--reasoning-effort` and codex has `model_reasoning_effort`, so
// reusing the gate set made C177 assert a fact the engine contradicts.
// A diagnostic that is wrong teaches authors to ignore it.
var reasoningEffortBackends = map[string]bool{
	"claude_code": true,
	"claw":        true,
	"pi":          true,
	"grok":        true,
	"codex":       true,
}

// clawBackendName is the literal value of the in-process backend.
// Hardcoded for the same reason as codexBackendName: ir must not import
// pkg/backend/delegate.
const clawBackendName = "claw"

// validateFallbacks checks every LLM node's `fallbacks:` block.
//
// Two of the checks are ERRORS rather than warnings, and both are cases
// where the degraded run would be silently WRONG rather than merely
// worse:
//
//   - a route that changes backend without pinning its own `model:` —
//     the model-spec forms are mutually incompatible (claw wants
//     `provider/model`, claude_code strips only an `anthropic/` prefix
//     and hard-fails on anything else), so the inherited model cannot
//     be valid on the new backend;
//   - a route that crosses the claw⇄CLI boundary on a node with an
//     EMPTY `tools:` list — the list inverts meaning there (empty means
//     zero tools on claw, and the FULL unrestricted native toolset on a
//     CLI backend under bypassPermissions), so a read-only reviewer
//     would become an agent able to edit the code it is judging.
//
// Everything else is a warning: the run proceeds, degraded but visible.
func (c *compiler) validateFallbacks(w *Workflow) {
	for _, n := range w.Nodes {
		nn, ok := n.(LLMNode)
		if !ok {
			continue
		}
		fbs := nn.GetFallbacks()
		if len(fbs) == 0 {
			continue
		}
		f := nn.GetLLMFields()
		kind, id := nn.NodeKind().String(), nn.NodeID()
		nodeBackend := effectiveNodeBackend(f.Backend, w.DefaultBackend)

		sandboxOff := sandboxExplicitlyOff(n, w)

		seen := map[string]bool{}
		for _, fb := range fbs {
			c.checkFallbackShape(kind, id, fb, seen)
			c.checkFallbackTriggers(kind, id, fb)
			c.checkFallbackCrossing(kind, id, fb, nn, nodeBackend, w.Permission, sandboxOff)
		}
	}
}

// checkFallbackShape validates the route on its own terms.
func (c *compiler) checkFallbackShape(kind, id string, fb Fallback, seen map[string]bool) {
	label := fallbackLabel(fb)
	if fb.Backend == "" && fb.Model == "" && fb.Provider == "" {
		c.errorfAt(DiagFallbackMalformed, id, "",
			"%s %q: fallback %s declares neither backend, model nor provider — it would re-issue the identical call that just failed",
			kind, id, label)
		return
	}
	if fb.Name != "" {
		if seen[fb.Name] {
			c.errorfAt(DiagFallbackMalformed, id, "",
				"%s %q: duplicate fallback name %q — names are the stable id a fall-through is reported by, so they must be unique",
				kind, id, fb.Name)
		}
		seen[fb.Name] = true
	}
	// A backend change with an inherited model cannot work: the
	// model-spec forms are mutually incompatible across backends.
	if fb.Backend != "" && fb.Model == "" && !strings.Contains(fb.Backend, "${") {
		c.errorfAt(DiagFallbackMalformed, id, "",
			"%s %q: fallback %s switches to backend %q without its own model: — model specs are not portable across backends (claw needs `provider/model`, claude_code accepts only a bare id or an `anthropic/` prefix)",
			kind, id, label, fb.Backend)
	}
}

// checkFallbackTriggers validates the `on:` filter vocabulary.
func (c *compiler) checkFallbackTriggers(kind, id string, fb Fallback) {
	for _, on := range fb.On {
		if strings.Contains(on, "${") {
			continue // resolved at run time
		}
		if !KnownFallbackTriggers[on] {
			c.warnfAt(DiagFallbackUnknownOn, id, "",
				"%s %q: fallback %s declares unknown trigger %q; known triggers are usage_window, auth, unavailable, transient_exhausted, any",
				kind, id, fallbackLabel(fb), on)
		}
	}
}

// checkFallbackCrossing validates what the route changes about the
// node's capabilities, using the same predicates the launch-time
// run-level route is screened by — so an operator cannot reach through
// `--fallback` a crossing the compiler refuses in the .bot.
func (c *compiler) checkFallbackCrossing(kind, id string, fb Fallback, nn LLMNode, nodeBackend, workflowPermission string, sandboxOff bool) {
	// An env-ref backend is not knowable here; defer to the runtime.
	if fb.Backend == "" || strings.Contains(fb.Backend, "${") {
		return
	}
	label := fallbackLabel(fb)
	permission := EffectivePermission(nn.GetPermission(), workflowPermission)

	// The permission gate is the anti-prompt-injection boundary. Whether
	// a ROUTE can enforce it is a property of the route's own backend,
	// so this check does NOT depend on the node's backend being
	// statically knowable — the auto-resolved shape is the shipped
	// default and must not escape it.
	if reason := ungatedCrossingReason(fb.Backend, permission); reason != "" {
		c.errorfAt(DiagFallbackUnsafeCross, id, "",
			"%s %q: fallback %s %s", kind, id, label, reason)
	}

	// claw enforces the gate IN-PROCESS only. Sandboxed, the policy does
	// not survive the IPC boundary (delegate.IOTask carries no
	// Permission field), so the backend REFUSES the node at execution
	// rather than run it ungated. Without this note the compiler would
	// green-light — via gateEnforcingBackends — precisely the route that
	// hard-fails, and it would fail at the worst possible moment: the
	// primary has just exhausted its quota and the chain is advancing.
	// A warning, not an error: unsandboxed claw genuinely enforces the
	// gate, and the sandbox default is applied outside the compiler
	// (product entry points), so this cannot be decided here.
	if !sandboxOff && permission != "" && permission != "off" && fb.Backend == clawBackendName {
		c.warnfAt(DiagFallbackDrift, id, "",
			"%s %q: fallback %s runs on claw, which can enforce the permission: %s gate only OUT of a sandbox; under the default `sandbox: auto` the route refuses at execution — declare `sandbox: none`, or route the fallback to claude_code/pi",
			kind, id, label, permission)
	}

	// The remaining checks compare the two backends, so they need the
	// node's own to be knowable.
	if nodeBackend == "" {
		return
	}
	if reason := toolsInversionReason(nodeBackend, fb.Backend, nn.GetTools()); reason != "" {
		c.errorfAt(DiagFallbackUnsafeCross, id, "",
			"%s %q: fallback %s %s", kind, id, label, reason)
	}

	// Reasoning effort degrades rather than misleads, so it warns.
	if effort := nn.GetLLMFields().ReasoningEffort; effort != "" && !reasoningEffortBackends[fb.Backend] {
		c.warnfAt(DiagFallbackDrift, id, "",
			"%s %q: fallback %s runs on backend %q, which has no reasoning-effort dial; the node's reasoning_effort: %s is ignored on that route",
			kind, id, label, fb.Backend, effort)
	}
}

// EffectivePermission resolves a node's gate mode: its own override
// wins, an empty one inherits the workflow block — which is the
// DOCUMENTED place to declare it, and therefore the shape a check that
// reads only the node-level field silently misses.
func EffectivePermission(nodePermission, workflowPermission string) string {
	if p := strings.TrimSpace(nodePermission); p != "" {
		return p
	}
	return strings.TrimSpace(workflowPermission)
}

// ungatedCrossingReason returns why a route may not serve a gated node,
// or "" when it may. Shared by C176 and the launch-time screen so the
// two can never disagree.
func ungatedCrossingReason(routeBackend, permission string) string {
	if permission == "" || permission == "off" || gateEnforcingBackends[routeBackend] {
		return ""
	}
	return fmt.Sprintf(
		"runs on backend %q, which cannot enforce the effective permission: %s gate — the run would fall back UNGATED",
		routeBackend, permission)
}

// toolsInversionReason returns why a route may not cross the claw⇄CLI
// boundary on this node, or "" when it may.
//
// The two directions are not symmetric:
//
//   - claw → CLI un-restricts the node WHATEVER it declared. Under the
//     always-on bypassPermissions a CLI agent ignores the lowercase
//     `tools:` list and carries the full native toolset, so a reviewer
//     restricted to read_file gains Edit/Write the moment the chain
//     falls through — and the parallel-branch admission was already
//     computed on the claw reading.
//   - CLI → claw restricts, which is only a hazard when the list is
//     EMPTY: on claw that means zero tools, so the node becomes a
//     schema-shaped narrator with no way to verify anything.
//
// Both backends must be statically known for the comparison to mean
// anything.
func toolsInversionReason(nodeBackend, routeBackend string, tools []string) string {
	if nodeBackend == "" || routeBackend == "" {
		return ""
	}
	nodeClaw := nodeBackend == clawBackendName
	routeClaw := routeBackend == clawBackendName
	if nodeClaw == routeClaw {
		return ""
	}
	if nodeClaw {
		return "routes a claw node to a CLI backend, which ignores the lowercase tools: list under bypassPermissions and always carries the full native toolset — the route silently un-restricts this node (and the parallel-branch admission was already decided on the claw reading); declare the node on the CLI backend instead, or route to another claw model"
	}
	if len(tools) > 0 {
		return ""
	}
	return "crosses the claw⇄CLI boundary on a node with no tools: list — an empty list means NO tools on claw but the full unrestricted toolset on a CLI backend, so the route silently changes what this node can do; declare an explicit tools: list"
}

// effectiveNodeBackend mirrors the runtime precedence knowable at
// compile time: the node's own `backend:` wins, an empty/`auto` one
// falls back to the workflow default. An env-ref or still-empty result
// is returned as "" — the literal text is not the resolved backend, so
// a check keyed on it would misfire.
//
// Copied from validateCommand rather than validateProviders, which
// reads f.Backend raw and therefore under-fires on every node that
// inherits `default_backend:`.
func effectiveNodeBackend(nodeBackend, workflowDefault string) string {
	backend := nodeBackend
	if backend == "" || backend == "auto" {
		backend = workflowDefault
	}
	if backend == "" || backend == "auto" || strings.Contains(backend, "${") {
		return ""
	}
	return backend
}

// fallbackLabel renders a route for a diagnostic message.
func fallbackLabel(fb Fallback) string {
	if fb.Name != "" {
		return fmt.Sprintf("%q", fb.Name)
	}
	return fmt.Sprintf("%q", strings.TrimSpace(fb.Backend+" "+fb.Model))
}

// sandboxExplicitlyOff reports whether this node is known, from the
// source alone, to run OUT of a sandbox — the node's own override if it
// has one, else the workflow block.
//
// It answers only "did the author opt out", never "will there be a
// sandbox": `sandbox: auto` is applied by the product entry points, not
// by the compiler, so an absent block reads as "probably sandboxed".
// That asymmetry is deliberate — a warning suppressed by a declaration
// the author actually wrote is safe; one suppressed by a default the
// compiler cannot see is not.
func sandboxExplicitlyOff(n Node, w *Workflow) bool {
	spec := w.Sandbox
	switch nn := n.(type) {
	case *AgentNode:
		if nn.Sandbox != nil {
			spec = nn.Sandbox
		}
	case *JudgeNode:
		if nn.Sandbox != nil {
			spec = nn.Sandbox
		}
	}
	return spec != nil && strings.TrimSpace(spec.Mode) == "none"
}
