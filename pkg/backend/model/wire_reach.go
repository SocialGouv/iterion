package model

import (
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
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
//   - a model-calling node that exposes no LLMFields (a subbot), a nil
//     workflow; a human node's LLM half is read on its model's prefix
//     (humanLLMModelOf) — on the wire when the prefix is, armed when the
//     prefix cannot be read; a Verified Action's recovery rungs are read
//     the way dispatch spends them (recoveryRungsOf): rung 3 as the direct
//     generation it is, rung 4 as the synthetic agent dispatch it is.
//
// A `{{vars.*}}` span resolves against the view's vars (RouteView.field) —
// the run's own reading — and a late name stays as written, undecided,
// exactly where the guard wants it.
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
func AnthropicWireReachable(v RouteView) bool {
	return len(AnthropicWireRoutes(v)) > 0
}

// WireRoute is one PRIMARY route of a run that can execute against the
// Anthropic wire: the node's backend under the launch's overrides and the
// chain its dispatch walks (Legs) — what the executor spends before any
// rescue step.
type WireRoute struct {
	// NodeID is the node the route belongs to; "" for the single route of a
	// nil workflow.
	NodeID string
	// Backend is the resolved backend, lower-cased; "" when it resolves at
	// dispatch (unset, auto, a `{{vars.…}}` reference) — a direct
	// generation's too, whose model alone routes.
	Backend string
	// Legs is the chain the dispatch walks (model.Legs — the override
	// collapse, the expansion, the allowlist's drop), in its order; empty
	// where the allowlist dropped every leg, which the dispatch refuses. A
	// direct generation is one leg whose hint is its model's prefix.
	Legs []credroute.Leg
	// Model is the spec a leg that pins none of its own sends: the node's
	// effective model (Baseline).
	Model string
	// Readable is false when the walk cannot say which credential the route
	// spends: a backend or first leg resolved at dispatch, a model it cannot
	// read, a model-calling node that exposes no LLM fields. As far as a
	// caller can tell, such a route draws on the run's DEFAULT credential.
	Readable bool
}

// Spec is the spec leg i sends: its own, else the route's model.
func (r WireRoute) Spec(i int) string {
	if i >= 0 && i < len(r.Legs) && r.Legs[i].Model != "" {
		return r.Legs[i].Model
	}
	return r.Model
}

// AnthropicWireRoutes lists the routes that make AnthropicWireReachable answer
// true — one per node, in node-id order, with what the walk can read of each.
// A nil workflow yields one unreadable route: the conservative reading.
//
// Each route's chain is the dispatch's own (Legs over the view), never a
// second parse of the `provider:` field: a reader keying the route reads the
// legs the dispatch walks, in the order it walks them. The fields are read
// through the view (RouteView.field): a `{{vars.*}}` span resolves against
// the run's vars the way dispatch resolves it, and a late name stays as
// written — undecided, which is the direction this guard answers in.
func AnthropicWireRoutes(v RouteView) []WireRoute {
	wf := v.Workflow
	if wf == nil {
		return []WireRoute{{}}
	}
	var out []WireRoute
	for _, n := range wf.Nodes {
		if _, ok := llmFieldsOf(n); !ok {
			if mdl, human := humanLLMModelOf(n); human {
				// The human node's LLM half (#2691): a direct generation on
				// its model — the prefix is the route, no hint and no backend
				// to pick a provider from. An anthropic-wire prefix is a
				// readable route on the wire, another prefix no route, and a
				// prefix the walk cannot read keeps the guard armed.
				if r := directWireRoute(n.NodeID(), v.field(mdl)); !r.Readable || anthropicWireProviders[r.Legs[0].Provider] {
					out = append(out, r)
				}
				continue
			}
			if spec, selfRepair, agentRung := recoveryRungsOf(n); selfRepair || agentRung {
				// A Verified Action's recovery ladder (#2783), read the way
				// dispatch spends it: rung 3 is a direct generation on the
				// recovery model (the human arm's reading), rung 4 a full
				// agent dispatch on the synthetic node — overrides and the
				// workflow's default backend included.
				spec = recoverySpecOf(v, spec)
				if selfRepair {
					if r := directWireRoute(n.NodeID()+"#self_repair", spec); !r.Readable || anthropicWireProviders[r.Legs[0].Provider] {
						out = append(out, r)
					}
				}
				if agentRung {
					syn := recoveryAgentNode(n.NodeID(), spec, nil)
					ov := v.Overrides.ForNode(syn.NodeID(), syn.NodeKind())
					backend := routeBackend(v.field(ov.Backend), "", v.field(wf.DefaultBackend))
					if r := nodeWireRoute(v, syn, backend); legsOnAnthropicWire(backend, r.Legs, r.Model) {
						r.NodeID = n.NodeID() + agentRecoverySiteSuffix
						out = append(out, r)
					}
				}
				continue
			}
			if ir.NodeUsesLLM(n) {
				out = append(out, WireRoute{NodeID: n.NodeID()})
			}
			continue
		}
		fields, _ := llmFieldsOf(n)
		ov := v.Overrides.ForNode(n.NodeID(), n.NodeKind())
		backend := routeBackend(v.field(ov.Backend), v.field(fields.Backend), v.field(wf.DefaultBackend))
		if r := nodeWireRoute(v, n, backend); legsOnAnthropicWire(backend, r.Legs, r.Model) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b WireRoute) int { return strings.Compare(a.NodeID, b.NodeID) })
	return out
}

// nodeWireRoute is n's route on backend: the chain the dispatch walks (Legs)
// and the model a leg that pins none sends (Baseline). Readable while the
// backend, the first leg and the model are decided: a later leg the walk
// reaches unreadable is the reader's to refuse, as the dispatch does.
func nodeWireRoute(v RouteView, n ir.Node, backend string) WireRoute {
	r := WireRoute{NodeID: n.NodeID(), Backend: backend, Legs: Legs(v, n), Model: strings.TrimSpace(Baseline(v, n))}
	r.Readable = backend != "" && !strings.Contains(r.Model, "{{")
	if len(r.Legs) > 0 {
		r.Readable = r.Readable && !strings.Contains(r.Legs[0].Provider, "{{") && !strings.Contains(r.Legs[0].Model, "{{")
	}
	return r
}

// legsOnAnthropicWire decides for one route whether it may execute against
// the anthropic wire, over the legs its dispatch walks: a backend that picks
// the wire itself, or resolves at dispatch, does; else a leg whose hint names
// a wire provider does (the hint IS the route — on pi it overrides the
// model's prefix, on claude_code the prefix is stripped); only a chain that
// names no hint at all routes on each leg's model `provider/` prefix. A leg
// that defers to the process (no hint, "auto") or reads a `{{vars.*}}` value
// leaves the chain unresolved, which keeps a backend that picks its provider
// from what the process holds on the wire. A chain the allowlist emptied is
// on no wire: the dispatch refuses the node.
func legsOnAnthropicWire(backend string, legs []credroute.Leg, base string) bool {
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
	named, unresolved := false, false
	for _, leg := range legs {
		hint := strings.ToLower(strings.TrimSpace(leg.Provider))
		switch {
		case hint == "":
			unresolved = true
		case anthropicWireProviders[hint]:
			return true
		case strings.Contains(hint, "{{"):
			named, unresolved = true, true
		default:
			named = true
		}
	}
	if !named {
		unresolved = false
		for _, leg := range legs {
			switch p := providerFromModelPrefix(firstNonEmpty(leg.Model, base)); {
			case p == "":
				unresolved = true
			case anthropicWireProviders[p]:
				return true
			}
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
