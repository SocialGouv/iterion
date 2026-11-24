package model

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
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
//     workflow; a human node's LLM half is read on its model's prefix — on
//     the wire when the prefix is, armed when the prefix cannot be read; a
//     Verified Action's recovery rungs are read the way dispatch spends them:
//     rung 3 as the direct generation it is, rung 4 as the synthetic agent
//     dispatch it is (CallSites, the one enumeration of the run's sites).
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
// Anthropic wire: one call site of the run (CallSites) — the node's backend
// under the launch's overrides and the chain its dispatch walks (Legs), or a
// direct generation's model — what the execution spends before any rescue.
type WireRoute struct {
	// NodeID is the route's site (CallSite.ID): the node's id for its own
	// dispatch, `<tool>#self_repair` / `<tool>#agent_recovery` for a
	// Verified Action's rungs; "" for the single route of a nil workflow.
	NodeID string
	// Backend is the resolved backend, lower-cased; "" when it resolves at
	// dispatch (unset, auto, a `{{vars.…}}` reference). A direct generation
	// runs on claw.
	Backend string
	// Legs is the chain the dispatch walks (model.Legs — the override
	// collapse, the expansion, the allowlist's drop), in its order; empty
	// where the allowlist dropped every leg, which the dispatch refuses. A
	// direct generation is one hint-free leg, as the call it makes.
	Legs []credroute.Leg
	// Model is the spec a leg that pins none sends: the node's effective
	// model (Baseline), a direct generation's own.
	Model string
	// Readable is false when the walk cannot say which credential the route
	// spends: a backend or first leg resolved at dispatch, a model it cannot
	// read, a model-calling node that exposes no LLM fields. As far as a
	// caller can tell, such a route draws on the run's DEFAULT credential.
	Readable bool
	// Role and Host are how the site calls (CallSite): a zero Role is the
	// node's own dispatch, Host a call this process makes on the host
	// whatever the run's sandbox — a direct generation.
	Role credroute.Role
	Host bool
}

// Spec is the spec leg i sends: its own, else the route's model.
func (r WireRoute) Spec(i int) string {
	return legSpec(r.Legs, i, r.Model)
}

// LegSpec is the spec leg i sends: its own, else base (Base).
func (r WireRoute) LegSpec(i int, base string) string {
	return legSpec(r.Legs, i, base)
}

// Base is the spec a leg of the route that pins none sends, as dispatch
// builds it (CallSite.Base): the route's model, the detector's pick for a
// claw dispatch naming none — its allowlist refusal the whole route's.
func (r WireRoute) Base(detected func(nodeID string) (string, error)) (string, error) {
	return siteBase(r.Model, r.Backend, r.Host, r.NodeID, detected)
}

// Call is leg i's question to the resolver for spec, placed where the
// route's call is made (CallSite.Call).
func (r WireRoute) Call(i int, spec string, placement credroute.Sandbox) credroute.Call {
	return siteCall(r.Backend, r.Legs, r.Role, r.Host, i, spec, placement)
}

// AnthropicWireRoutes lists the routes that make AnthropicWireReachable answer
// true — one per primary call site of the run (CallSites), in site-id order,
// with what the walk can read of each. A nil workflow yields one unreadable
// route: the conservative reading.
//
// The sites are the enumeration the launch's admission judges, never a second
// walk over the node kinds: each route's chain is the dispatch's own (Legs over
// the view), a leg reader keys the legs the dispatch walks in the order it
// walks them, and a `{{vars.*}}` span resolves against the run's vars the way
// dispatch resolves it — a late name stays as written, undecided, which is the
// direction this guard answers in. A rescue site (a `fallbacks:` route, an
// interaction generation, the review companion) is no primary route, and a
// supervisor is the pre-flight's own walk over the same enumeration.
func AnthropicWireRoutes(v RouteView) []WireRoute {
	if v.Workflow == nil {
		return []WireRoute{{}}
	}
	var out []WireRoute
	for _, s := range CallSites(v, nil) {
		if s.Rescue {
			continue
		}
		if r := s.wireRoute(); r.onAnthropicWire() {
			out = append(out, r)
		}
	}
	return out
}

// wireRoute is s as the pre-flight reads it. Readable while the backend, the
// first leg and the model are decided — a direct generation's while its
// model's prefix is; a later leg the walk reaches unreadable is the reader's
// to refuse, as the dispatch does.
func (s CallSite) wireRoute() WireRoute {
	r := WireRoute{NodeID: s.ID, Backend: s.Backend, Legs: s.Legs, Model: s.Model, Role: s.Role, Host: s.Host}
	if r.Role == credroute.RoleNode {
		r.Role = "" // the zero Role is the node's own dispatch
	}
	switch {
	case s.Kind == SiteUnread:
		r.Backend, r.Legs, r.Role = "", nil, ""
	case s.Host:
		r.Readable = providerFromModelPrefix(s.Model) != ""
	default:
		r.Readable = s.Backend != "" && !strings.Contains(s.Model, "{{")
		if len(s.Legs) > 0 {
			r.Readable = r.Readable && !strings.Contains(s.Legs[0].Provider, "{{") && !strings.Contains(s.Legs[0].Model, "{{")
		}
	}
	return r
}

// onAnthropicWire reports whether the route may execute against the
// anthropic wire: an unreadable route keeps the guard armed, a direct
// generation is on the wire its model's prefix names, a dispatch where its
// legs put it (legsOnAnthropicWire).
func (r WireRoute) onAnthropicWire() bool {
	if !r.Readable && (r.Host || len(r.Legs) == 0 && r.Backend == "") {
		return true
	}
	return legsOnAnthropicWire(r.Backend, r.Legs, r.Model)
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
