package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-131 §4, the dispatch-side belt of the policy-ladder screen: under
// team_gateway a node whose own route — its resolved `model:`, or a step of
// its `provider:` chain — goes to the team's gateway walks no policy rung,
// whatever the screen landed. The screen runs at executor build over the
// message's vars; a resume restores the launch's vars into the executor
// after it (pushExecutorVars), so a `{{vars.model}}` the launch set to the
// gateway over a vendor default carries the rungs the screen mapped from the
// default. The operator's explicit run-level route (RunStageSet, not Policy)
// stays, and a vendor-routed node or an unstamped run keeps its rungs.
func TestResolveChain_TheContentBoundaryDropsPolicyRungsOffAGatewayNode(t *testing.T) {
	t.Parallel()
	policyRung := ir.Fallback{Name: ir.RunFallbackName, Backend: "claw", Model: "anthropic/claude-opus-5", Provider: "anthropic", RunStage: 1, RunStageSet: true, Policy: true}
	operatorRung := ir.Fallback{Name: ir.RunFallbackName, Backend: "claw", Model: "openai/gpt-6-sol", RunStage: 0, RunStageSet: true}
	gatewayVars := map[string]any{"model": "openai_compatible/scaleway/glm-5.3"}
	for _, row := range []struct {
		name        string
		model       string
		provider    string
		vars        map[string]any
		boundary    *store.RunContentBoundary
		wantLabeled int // chain elements past the node's own route
	}{
		{name: "{{vars.model}} the dispatch vars route to the gateway", model: "{{vars.model}}", vars: gatewayVars,
			boundary: &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway}, wantLabeled: 1},
		{name: "a gateway step in the provider chain", model: "anthropic/claude-opus-5", provider: "openai_compatible:openai_compatible/scaleway/glm-5.3",
			boundary: &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway}, wantLabeled: 1},
		{name: "a vendor node keeps its rung (control)", model: "{{vars.model}}", vars: map[string]any{"model": "anthropic/claude-sonnet-4-6"},
			boundary: &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway}, wantLabeled: 2},
		{name: "own_keys keeps the rung (control)", model: "{{vars.model}}", vars: gatewayVars,
			boundary: &store.RunContentBoundary{Kind: store.ContentBoundaryOwnKeys}, wantLabeled: 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			e := &ClawExecutor{logger: iterlog.Nop(), vars: row.vars}
			WithContentBoundary(row.boundary)(e)
			node := chainAgentNode("gw", "claw", row.provider, []ir.Fallback{operatorRung, policyRung})
			node.Model = row.model
			var labeled []chainElement
			for _, el := range e.resolveChain(node) {
				if el.Label != "" {
					labeled = append(labeled, el)
				}
			}
			if len(labeled) != row.wantLabeled {
				t.Fatalf("routes past the node's own = %+v, want %d", labeled, row.wantLabeled)
			}
			if labeled[0].Model != operatorRung.Model {
				t.Fatalf("first route = %+v, want the operator's explicit run-level route kept", labeled[0])
			}
		})
	}
}
