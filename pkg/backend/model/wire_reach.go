package model

import (
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// anthropicWireProviders are the credential-routing hints that spend an
// Anthropic-wire credential: the direct API key / OAuth forfait, and the
// facades that ride the same wire. Derived from the slot order rather than
// listed, so a provider joining the wire arms this guard the same day it
// becomes spendable — a hint missing here makes the pre-flight answer "this
// run cannot touch the capped subscription" for a run that can.
var anthropicWireProviders = anthropicWireHints()

func anthropicWireHints() map[string]bool {
	hints := map[string]bool{}
	for _, slot := range secrets.AnthropicWireSlotOrder {
		if secrets.OAuthKind(slot).Valid() {
			continue // an OAuth kind is not a `provider:` hint
		}
		hints[slot] = true
	}
	return hints
}

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
// codex/kimi/grok, or claw/pi/opencode with a resolved non-anthropic
// provider — answers false. opencode sits with claw and pi rather than with
// the vendor-bound CLIs: it picks its provider from what the process holds,
// so an unresolved hint may still land on anthropic.
func AnthropicWireReachable(wf *ir.Workflow, overrides ModelOverrides) bool {
	return len(AnthropicWireRoutes(wf, overrides)) > 0
}

// WireRoute is one PRIMARY route of a run that can execute against the
// Anthropic wire: the node's backend under the launch's overrides and the
// FIRST element of its provider chain — what the executor spends before any
// rescue step.
type WireRoute struct {
	// NodeID is the node the route belongs to; "" for the single route of a
	// nil workflow.
	NodeID string
	// Backend is the resolved backend, lower-cased; "" when it resolves at
	// dispatch (unset, auto, a `{{vars.…}}` reference).
	Backend string
	// Hint is the first chain element's provider hint; "" for none or auto.
	Hint string
	// Model is the first element's own model, else the node's effective one.
	Model string
	// Readable is false when the walk cannot say which credential the route
	// spends: a backend or hint resolved at dispatch, a model it cannot read,
	// a model-calling node that exposes no LLM fields. As far as a caller can
	// tell, such a route draws on the run's DEFAULT credential.
	Readable bool
}

// AnthropicWireRoutes lists the routes that make AnthropicWireReachable answer
// true — one per node, in node-id order, with what the walk can read of each.
// A nil workflow yields one unreadable route: the conservative reading.
func AnthropicWireRoutes(wf *ir.Workflow, overrides ModelOverrides) []WireRoute {
	if wf == nil {
		return []WireRoute{{}}
	}
	var out []WireRoute
	for _, n := range wf.Nodes {
		fields, ok := llmFieldsOf(n)
		if !ok {
			if ir.NodeUsesLLM(n) {
				out = append(out, WireRoute{NodeID: n.NodeID()})
			}
			continue
		}
		ov := overrides.ForNode(n.NodeID(), n.NodeKind())
		backend := routeBackend(ov.Backend, fields.Backend, wf.DefaultBackend)
		mdl := firstNonEmpty(ov.Model, fields.Model)
		if routeOnAnthropicWire(backend, ov.Provider, fields.Provider, mdl) {
			out = append(out, primaryWireRoute(n.NodeID(), backend, ov.Provider, fields.Provider, mdl))
		}
	}
	slices.SortFunc(out, func(a, b WireRoute) int { return strings.Compare(a.NodeID, b.NodeID) })
	return out
}

// primaryWireRoute reads a route's first chain element the way the executor
// builds it (ClawExecutor.resolveProviderChain): a launch-time provider
// override is the whole chain; else the DSL chain, env-expanded and split on
// commas, whose first `provider[:model]` token leads; "auto" is no hint.
func primaryWireRoute(id, backend, overrideProvider, chain, mdl string) WireRoute {
	r := WireRoute{NodeID: id, Backend: backend, Model: strings.TrimSpace(ir.ExpandEnvWithDefault(mdl))}
	if strings.TrimSpace(overrideProvider) != "" {
		r.Hint = strings.TrimSpace(overrideProvider)
	} else {
		for _, part := range strings.Split(ir.ExpandEnvWithDefault(chain), ",") {
			token := strings.TrimSpace(part)
			if token == "" {
				continue
			}
			hint, model, _ := ir.SplitProviderStep(token)
			r.Hint = strings.TrimSpace(hint)
			if m := strings.TrimSpace(model); m != "" {
				r.Model = m
			}
			break
		}
	}
	if strings.EqualFold(r.Hint, "auto") {
		r.Hint = ""
	}
	r.Readable = backend != "" && !strings.Contains(r.Hint, "{{") && !strings.Contains(r.Model, "{{")
	return r
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
	// A `{{vars.…}}` backend resolves at dispatch, with the run's vars this
	// pre-flight does not have: it may be claude_code or claw on anthropic,
	// so the route stays reachable — the conservative side of this guard.
	if strings.Contains(backend, "{{") {
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
