package model

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// GatewaySites and DeferredSites: the sites whose route rides the
// openai_compatible gateway, and those whose route the DISPATCHING process
// decides — a deciding field read from the environment the runner expands
// with its own, or from a run var this view left late. One row per recording
// site of the walk, each isolated: a mixed workflow keeps a literal vendor
// route beside it that must stay out of both lists.
//
// Mutations, each red on its own rows and restored byte-identical:
//   - deferAt reading no field (`late := a.backendLate` only) → every
//     field-deferred row loses its site;
//   - resolveNode deciding on the chain under a provider override → "an
//     override collapses an env chain" records the node;
//   - the node's backendLate left false → "a backend read from the
//     environment" and "a run-level stage on the nodes' own backend" lose
//     their sites;
//   - prefixOrWiden's gatewayAt dropped → "a literal gateway model" and "an
//     env model that expanded to the gateway" lose the gateway site;
//   - chainDecides' gatewayAt dropped → "a gateway hint" loses it.
func TestEffectiveProvidersGatewayAndDeferredSites(t *testing.T) {
	known := map[string]bool{"anthropic": true, "openai": true, "zai": true}
	t.Setenv("ROUTE_SITES_GATEWAY_MODEL", "openai_compatible/glm-5.3")
	agent := func(id, backend, provider, model string) *ir.AgentNode {
		return &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, LLMFields: ir.LLMFields{Backend: backend, Provider: provider, Model: model}}
	}
	// The literal vendor route every row carries beside its own.
	vendor := agent("literal", "claw", "", "anthropic/claude-opus-5")
	wfOf := func(nodes ...*ir.AgentNode) *ir.Workflow {
		wf := &ir.Workflow{Nodes: map[string]ir.Node{}}
		for _, n := range append([]*ir.AgentNode{vendor}, nodes...) {
			wf.Nodes[n.ID] = n
		}
		return wf
	}
	providerOverride := ModelOverrides{}
	providerOverride.SetProvider("draft", "anthropic")
	for _, tc := range []struct {
		name         string
		view         RouteView
		wantGateway  []string
		wantDeferred []string
	}{
		{name: "literal vendor routes only",
			view: RouteView{Workflow: wfOf(agent("draft", "claw", "openai", "openai/gpt-6"))}},
		{name: "a literal gateway model",
			view:        RouteView{Workflow: wfOf(agent("draft", "claw", "", "openai_compatible/glm-5.3"))},
			wantGateway: []string{"draft"}},
		// On claude_code: claw would also read the model's prefix
		// (prefixSlotForClaw), and the hint's own recording must stand alone.
		{name: "a gateway hint over a gateway model",
			view:        RouteView{Workflow: wfOf(agent("draft", "claude_code", "openai_compatible", "openai_compatible/glm-5.3"))},
			wantGateway: []string{"draft"}},
		{name: "a model read from the environment",
			view:         RouteView{Workflow: wfOf(agent("draft", "claw", "", "${ROUTE_SITES_UNSET_MODEL:-anthropic/claude-opus-5}"))},
			wantDeferred: []string{"draft"}},
		{name: "an env model that expanded to the gateway",
			view:         RouteView{Workflow: wfOf(agent("draft", "claw", "", "${ROUTE_SITES_GATEWAY_MODEL:-anthropic/claude-opus-5}"))},
			wantGateway:  []string{"draft"},
			wantDeferred: []string{"draft"}},
		{name: "a provider chain read from the environment",
			view:         RouteView{Workflow: wfOf(agent("draft", "claw", "${ROUTE_SITES_UNSET_PROVIDER:-}", "anthropic/claude-opus-5"))},
			wantDeferred: []string{"draft"}},
		{name: "an override collapses an env chain",
			view: RouteView{Workflow: wfOf(agent("draft", "claw", "${ROUTE_SITES_UNSET_PROVIDER:-}", "anthropic/claude-opus-5")), Overrides: providerOverride}},
		{name: "a backend read from the environment",
			view:         RouteView{Workflow: wfOf(agent("draft", "${ROUTE_SITES_UNSET_BACKEND:-claude_code}", "anthropic", "claude-opus-5-5"))},
			wantDeferred: []string{"draft"}},
		{name: "a model from a late run var",
			view:         RouteView{Workflow: wfOf(agent("draft", "claw", "", "{{vars.model}}")), LateVars: map[string]bool{"model": true}},
			wantDeferred: []string{"draft"}},
		{name: "an authored rescue read from the environment",
			view: RouteView{Workflow: wfOf(&ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "draft"},
				LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"},
				Fallbacks: []ir.Fallback{{Name: "glm", Provider: "zai", Model: "${ROUTE_SITES_UNSET_RESCUE:-glm-5.3}"}},
			})},
			wantDeferred: []string{"draft#glm"}},
		{name: "a supervisor read from the environment",
			view: func() RouteView {
				wf := wfOf()
				wf.Supervisors = []*ir.Supervisor{{Name: "watch", Model: "${ROUTE_SITES_UNSET_SUPERVISOR:-anthropic/claude-opus-5}"}}
				return RouteView{Workflow: wf}
			}(),
			wantDeferred: []string{"supervisor:watch"}},
		{name: "a run-level stage on the nodes' own backend",
			view: RouteView{
				Workflow:     wfOf(agent("draft", "${ROUTE_SITES_UNSET_BACKEND:-claw}", "", "anthropic/claude-opus-5")),
				RunFallbacks: []FallbackEntry{{Model: "openai/gpt-6"}},
			},
			wantDeferred: []string{"draft", ir.RunFallbackName + "#0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := EffectiveProviders(tc.view, known)
			if !slices.Equal(res.GatewaySites, tc.wantGateway) {
				t.Errorf("GatewaySites = %v, want %v", res.GatewaySites, tc.wantGateway)
			}
			if !slices.Equal(res.DeferredSites, tc.wantDeferred) {
				t.Errorf("DeferredSites = %v, want %v", res.DeferredSites, tc.wantDeferred)
			}
			// The literal vendor route is decided and attributed in every row.
			if !slices.Contains(res.Providers, "anthropic") || slices.Contains(res.DeferredSites, "literal") || slices.Contains(res.GatewaySites, "literal") {
				t.Errorf("the literal vendor route: Providers=%v GatewaySites=%v DeferredSites=%v — it is decided here and rides no gateway", res.Providers, res.GatewaySites, res.DeferredSites)
			}
		})
	}
}
