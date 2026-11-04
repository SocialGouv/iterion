package model

import (
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// chainReading is the one reading of a node's routing fields every reader of
// its chain shares (ADR-161 decision 2): the launch-time overrides, the vars a
// `{{vars.*}}` span resolves against — and the late ones that stay as written
// —, and the run's provider allowlist. The executor reads it with its own
// fields (chainReading), a launch-time reader with a RouteView's
// (RouteView.chainReading): one code path, so dispatch and admission walk the
// same chain.
type chainReading struct {
	overrides ModelOverrides
	vars      map[string]any
	late      map[string]bool
	allowed   map[string]bool
}

// chainReading is the executor's own reading.
func (e *ClawExecutor) chainReading() chainReading {
	return chainReading{overrides: e.modelOverrides, vars: e.vars, allowed: e.allowedProviders}
}

// chainReading is the view's reading: its overrides, vars and allowlist.
func (v RouteView) chainReading() chainReading {
	return chainReading{overrides: v.Overrides, vars: v.Vars, late: v.LateVars, allowed: AllowedProviderSet(v.AllowedProviders)}
}

// field is the dispatch reading of a routing field: template, environment,
// trim (ResolveRoutingFieldWith).
func (c chainReading) field(raw string) string {
	return ResolveRoutingFieldWith(c.vars, c.late, raw)
}

// baselineModel is the model an element that pins none of its own runs: the
// launch override if the operator set one, else the node's `model:` with its
// references resolved.
func (c chainReading) baselineModel(node ir.Node) string {
	if ov := c.overrides.ForNode(node.NodeID(), node.NodeKind()); ov.Model != "" {
		return ov.Model
	}
	var m string
	switch n := node.(type) {
	case *ir.AgentNode:
		m = n.Model
	case *ir.JudgeNode:
		m = n.Model
	case *ir.RouterNode:
		m = n.Model
	}
	return c.field(m)
}

// providerChain resolves the per-node `provider:` field into the ordered
// chain dispatch walks: a launch-time provider override collapses the node's
// own field (both read through the ONE parse, parseProviderChain), the field
// is expanded, parsed, and the allowlist applied (dropForbidden). See
// ClawExecutor.resolveProviderChain for the vocabulary.
func (c chainReading) providerChain(node ir.Node) []chainElement {
	// A launch-time provider override (a routing-policy cell or the
	// launch's own rows) carries the same vocabulary as the node's own
	// field — a validated cell may name a multi-step chain or a
	// `provider:model` step — so it is read through the ONE parse below.
	// Returning the raw string as a single element handed dispatch a
	// provider name no credential family matches, while the write gate
	// and the credential preview certified each step of the chain.
	if ov := c.overrides.ForNode(node.NodeID(), node.NodeKind()); ov.Provider != "" {
		return c.dropForbidden(node, parseProviderChain(c.field(ov.Provider)))
	}
	var raw string
	switch n := node.(type) {
	case *ir.AgentNode:
		raw = n.Provider
	case *ir.JudgeNode:
		raw = n.Provider
	case *ir.RouterNode:
		raw = n.Provider
	}
	return c.dropForbidden(node, parseProviderChain(c.field(raw)))
}

// dropForbidden drops every chain element whose resolved provider is outside
// the run's allowlist — see ClawExecutor.dropForbidden.
func (c chainReading) dropForbidden(node ir.Node, chain []chainElement) []chainElement {
	if c.allowed == nil {
		return chain
	}
	base := c.baselineModel(node)
	out := make([]chainElement, 0, len(chain))
	dropped := false
	for _, el := range chain {
		p := strings.ToLower(strings.TrimSpace(el.Provider))
		if p == "" {
			m := el.Model
			if m == "" {
				m = base
			}
			p = providerOfModelSpec(m)
		}
		if p != "" && !c.allowed[p] {
			dropped = true
			continue
		}
		out = append(out, el)
	}
	if !dropped {
		return chain
	}
	return out
}

// Legs is the chain dispatch walks for node under v (ADR-161 decision 2):
// the executor's own resolveProviderChain, exported beside the view —
// override collapse, expansion, the allowlist's drop. Dispatch, the
// pre-flight and the launch's admission read it, and no reader parses the
// `provider:` field another way. Empty when the allowlist dropped every leg:
// there is no route, and dispatch refuses the node the same way.
func Legs(v RouteView, node ir.Node) []credroute.Leg {
	return legsOf(v.chainReading().providerChain(node))
}

// Baseline is the model a leg that pins none of its own sends under v: the
// launch override, else the node's `model:` with its references resolved.
// Empty where the node names none — claw's dispatch then picks the
// detector's suggestion.
func Baseline(v RouteView, node ir.Node) string {
	return v.chainReading().baselineModel(node)
}

// legsOf renders a chain in the resolver's vocabulary.
func legsOf(chain []chainElement) []credroute.Leg {
	out := make([]credroute.Leg, 0, len(chain))
	for _, el := range chain {
		out = append(out, credroute.Leg{Provider: el.Provider, Model: el.Model})
	}
	return out
}

// CredentialRouteView is the view this executor's dispatch reads wf's chains
// under — its launch overrides, its vars and its allowlist — for a reader
// that judges the same chains before any node dispatches: the launch's
// credential admission (ADR-161 decision 4).
func (e *ClawExecutor) CredentialRouteView(wf *ir.Workflow) RouteView {
	var allowed []string
	for name := range e.allowedProviders {
		allowed = append(allowed, name)
	}
	slices.Sort(allowed)
	return RouteView{Workflow: wf, Overrides: e.modelOverrides, Vars: e.vars, AllowedProviders: allowed}
}
