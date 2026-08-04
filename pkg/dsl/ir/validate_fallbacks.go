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

		seen := map[string]bool{}
		for _, fb := range fbs {
			c.checkFallbackShape(kind, id, fb, seen)
			c.checkFallbackTriggers(kind, id, fb)
			c.checkFallbackCrossing(kind, id, fb, nn, nodeBackend)
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
// node's capabilities.
func (c *compiler) checkFallbackCrossing(kind, id string, fb Fallback, nn LLMNode, nodeBackend string) {
	// An env-ref backend is not knowable here; defer to the runtime.
	if fb.Backend == "" || strings.Contains(fb.Backend, "${") || nodeBackend == "" {
		return
	}
	label := fallbackLabel(fb)

	// The permission gate is the anti-prompt-injection boundary. A route
	// that cannot enforce it must not exist, rather than silently
	// running the node ungated.
	perm := nn.GetPermission()
	if perm != "" && perm != "off" && !gateEnforcingBackends[fb.Backend] {
		c.errorfAt(DiagFallbackUnsafeCross, id, "",
			"%s %q: fallback %s runs on backend %q, which cannot enforce this node's permission: %s gate — the run would fall back UNGATED",
			kind, id, label, fb.Backend, perm)
	}

	// `tools:` inverts meaning across the claw⇄CLI boundary: empty means
	// ZERO tools on claw and the FULL native toolset on a CLI backend.
	crossesClawBoundary := (nodeBackend == clawBackendName) != (fb.Backend == clawBackendName)
	if crossesClawBoundary && len(nn.GetTools()) == 0 {
		c.errorfAt(DiagFallbackUnsafeCross, id, "",
			"%s %q: fallback %s crosses the claw⇄CLI boundary on a node with no tools: list — an empty list means NO tools on claw but the full unrestricted toolset on a CLI backend, so the route silently changes what this node can do; declare an explicit tools: list",
			kind, id, label)
	}

	// Reasoning effort, compression and memory are honoured unevenly.
	// These degrade rather than mislead, so they warn.
	if effort := nn.GetLLMFields().ReasoningEffort; effort != "" && !gateEnforcingBackends[fb.Backend] {
		c.warnfAt(DiagFallbackDrift, id, "",
			"%s %q: fallback %s runs on backend %q, which has no reasoning-effort dial; the node's reasoning_effort: %s is ignored on that route",
			kind, id, label, fb.Backend, effort)
	}
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
