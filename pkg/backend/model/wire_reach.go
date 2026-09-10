package model

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// anthropicWireProviders are the credential-routing hints that spend an
// Anthropic-wire credential: the direct API key / OAuth forfait, and the
// z.ai facade that rides the same wire.
var anthropicWireProviders = map[string]bool{"anthropic": true, "zai": true}

// AnthropicWireReachable reports whether some route the run may take can
// execute against the Anthropic wire — the wire the operator's usage cap
// meters (its readings come from the claude_code delegate's own session
// telemetry and nowhere else). A pre-flight guard that refuses a run in
// advance must ask this: a run whose every route is claw/openai cannot
// spend the capped subscription, and parking it for the anthropic weekly
// reset protects nothing (#668).
//
// CONSERVATIVE in the direction that matters — every uncertainty answers
// true, keeping the guard armed:
//   - a backend that is claude_code, empty or "auto" (the resolver may
//     pick claude_code from the host's credentials);
//   - claw or pi with a provider it cannot resolve (claw substitutes the
//     first available provider, which may be anthropic);
//   - any resolved hint that is anthropic or zai, on any backend;
//   - a model-calling node that exposes no LLMFields (human answering
//     with a model, subbot, agent-rung recovery), a nil workflow.
//
// PRIMARY routes only. A `fallbacks:` route onto the wire does not arm
// the guard: the primary can carry the whole run without ever touching
// it, and the rescue route only fires on a failure the mid-run guard and
// the delegate's own usage-window classification already refuse at
// dispatch. Arming on it would refuse IN ADVANCE a run that could not
// possibly spend the capped subscription — the rule this pre-flight
// exists to honour.
//
// Only a run whose every primary route is pinned off the wire —
// codex/kimi/grok, or claw/pi with a resolved non-anthropic provider —
// answers false.
func AnthropicWireReachable(wf *ir.Workflow, overrides ModelOverrides) bool {
	if wf == nil {
		return true
	}
	wfBackend := strings.TrimSpace(ir.ExpandEnvWithDefault(wf.DefaultBackend))
	for _, n := range wf.Nodes {
		fields, ok := llmFieldsOf(n)
		if !ok {
			if ir.NodeUsesLLM(n) {
				return true
			}
			continue
		}
		ov := overrides.ForNode(n.NodeID(), n.NodeKind())
		backend := firstNonEmpty(strings.TrimSpace(ov.Backend), strings.TrimSpace(ir.ExpandEnvWithDefault(fields.Backend)), wfBackend)
		mdl := firstNonEmpty(ov.Model, fields.Model)
		if routeOnAnthropicWire(backend, ov.Provider, fields.Provider, mdl) {
			return true
		}
	}
	return false
}

// routeOnAnthropicWire decides for one route, in the executor's
// precedence: a provider override collapses the chain; else the DSL chain
// decides when it names a hint (the hint IS the route — on pi the hint
// overrides the model's prefix, on claude_code the prefix is stripped);
// only a route with no hint at all routes on its model's `provider/`
// prefix.
func routeOnAnthropicWire(backend, overrideProvider, chain, mdl string) bool {
	backend = strings.ToLower(backend)
	switch backend {
	case "", "auto", delegate.BackendClaudeCode:
		return true
	}
	var hints []string
	unresolved := false
	switch {
	case strings.TrimSpace(overrideProvider) != "":
		hints, unresolved = chainHints(overrideProvider)
	default:
		hints, unresolved = chainHints(chain)
		if len(hints) == 0 {
			if p := providerFromModelPrefix(mdl); p != "" {
				hints, unresolved = []string{p}, false
			}
		}
	}
	for _, h := range hints {
		if anthropicWireProviders[h] {
			return true
		}
	}
	if !unresolved {
		return false
	}
	// Unresolved on a backend that picks its provider from what the
	// process holds: may land on anthropic. The CLI backends bound to one
	// vendor cannot.
	switch backend {
	case delegate.BackendCodex, delegate.BackendKimi, delegate.BackendGrok:
		return false
	}
	return true
}
