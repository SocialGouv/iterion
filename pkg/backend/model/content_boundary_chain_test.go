package model

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
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

// The belt and the policy-ladder screen judge one route with one predicate
// (ir.RouteNamesGateway): on every spelling the screen holds a node to the
// gateway (a case variant, a space before the slash, an empty id, a step model
// or hint spelled so, a gateway `model:` whose steps each pin their own), the
// belt drops the node's policy rungs too. A spelling dispatch cannot serve
// fails the primary with an untyped error, which reaches a rung past any `on:`
// filter — the vendor credential the boundary keeps the node from. Each row
// asserts both verdicts; the vendor rows are the control.
func TestResolveChain_TheBeltHoldsEveryRouteTheScreenHolds(t *testing.T) {
	t.Parallel()
	policyRung := ir.Fallback{Name: ir.RunFallbackName, Backend: "claw", Model: "anthropic/claude-opus-5", Provider: "anthropic", On: []string{"usage_window"}, RunStage: 0, RunStageSet: true, Policy: true}
	wf := &ir.Workflow{Vars: map[string]*ir.Var{"model": {Name: "model", Type: ir.VarString, HasDefault: true, Default: "anthropic/claude-opus-5"}}}
	for _, row := range []struct {
		name, model, provider, value string
		held                         bool
	}{
		{name: "case variant", model: "{{vars.model}}", value: "OpenAI_Compatible/team/m", held: true},
		{name: "space before the slash", model: "{{vars.model}}", value: "openai_compatible /team/m", held: true},
		{name: "empty id", model: "{{vars.model}}", value: "openai_compatible/", held: true},
		{name: "a step model, case variant", model: "anthropic/claude-opus-5", provider: "zai:{{vars.model}}", value: "OpenAI_Compatible/team/m", held: true},
		{name: "a step hint, case variant", model: "anthropic/claude-opus-5", provider: "{{vars.model}}", value: " OpenAI_Compatible ", held: true},
		{name: "a gateway model whose every step pins its own", model: "{{vars.model}}", provider: "zai:glm-5.3", value: "openai_compatible/team/m", held: true},
		{name: "the canonical spelling", model: "{{vars.model}}", value: "openai_compatible/team/m", held: true},
		{name: "a vendor route (control)", model: "{{vars.model}}", value: "anthropic/claude-sonnet-4-6"},
		{name: "a prefix that only starts like the gateway's (control)", model: "{{vars.model}}", value: "openai_compatible_x/team/m"},
		{name: "a vendor step under a vendor model (control)", model: "anthropic/claude-opus-5", provider: "zai:{{vars.model}}", value: "glm-5.3"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			reading := ir.NewPolicyLadderReading(wf, map[string]string{"model": row.value})
			_, landed, reason := reading.StageModel("claw", "anthropic_key", row.model, row.provider, llmroute.ClassTable{}, true)
			if screenHeld := !landed && reason == llmroute.ReasonContentBoundary; screenHeld != row.held {
				t.Fatalf("screen: landed=%v reason=%q, want held=%v", landed, reason, row.held)
			}
			e := &ClawExecutor{logger: iterlog.Nop(), vars: map[string]any{"model": row.value}}
			WithContentBoundary(&store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway})(e)
			node := chainAgentNode("gw", "claw", row.provider, []ir.Fallback{policyRung})
			node.Model = row.model
			kept := slices.ContainsFunc(e.resolveChain(node), func(el chainElement) bool { return el.Label != "" })
			if kept == row.held {
				t.Fatalf("belt: policy rung kept=%v on %q (model %q, provider %q), want held=%v — the screen's verdict", kept, row.value, row.model, row.provider, row.held)
			}
		})
	}
}

// A launch-time MODEL override is a routing field like the node's own
// `model:`: the screen reads it through the launch override first
// (ModelOverrides.LadderRoute) and resolves it — vars, environment, trim — so
// the belt resolves it the same way before judging whether the node's own
// route goes to the gateway. On a resume the screen runs on the declared
// defaults (a vendor route: the rung lands) and the belt on the restored vars
// (the gateway): read raw, the override's `{{vars.model}}` named no gateway,
// the belt kept the rung, the primary failed on the unresolved spec and the
// rung took the node's content to the vendor.
func TestResolveChain_TheBeltResolvesALaunchModelOverrideAsTheScreenDoes(t *testing.T) {
	t.Parallel()
	policyRung := ir.Fallback{Name: ir.RunFallbackName, Backend: "claw", Model: "anthropic/claude-opus-5", Provider: "anthropic", On: []string{"usage_window"}, RunStage: 0, RunStageSet: true, Policy: true}
	wf := &ir.Workflow{Vars: map[string]*ir.Var{"model": {Name: "model", Type: ir.VarString, HasDefault: true, Default: "anthropic/claude-opus-5"}}}
	gateway := map[string]string{"model": "openai_compatible/team/m"}
	vendor := map[string]string{"model": "anthropic/claude-sonnet-4-6"}
	for _, row := range []struct {
		name, override string
		screenVars     map[string]string // nil: the declared defaults (a resume's screen)
		beltVars       map[string]string
		screenHeld     bool
		beltHeld       bool
	}{
		{name: "launch: {{vars.model}} override set to the gateway", override: "{{vars.model}}", screenVars: gateway, beltVars: gateway, screenHeld: true, beltHeld: true},
		{name: "resume: the screen on the defaults, the belt on the restored gateway var", override: "{{vars.model}}", beltVars: gateway, beltHeld: true},
		{name: "an env-dial override defaulting to the gateway", override: "${ITERION_TEST_UNSET_OVERRIDE_DIAL:-openai_compatible/team/d}", screenVars: vendor, beltVars: vendor, screenHeld: true, beltHeld: true},
		{name: "a vendor override value (control)", override: "{{vars.model}}", screenVars: vendor, beltVars: vendor},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var ov ModelOverrides
			ov.SetModel("*", row.override)
			node := chainAgentNode("gw", "claw", "", []ir.Fallback{policyRung})
			node.Model = "anthropic/claude-opus-5"
			lm, lp := ov.LadderRoute(node)
			_, landed, reason := ir.NewPolicyLadderReading(wf, row.screenVars).StageModel("claw", "anthropic_key", lm, lp, llmroute.ClassTable{}, true)
			if screenHeld := !landed && reason == llmroute.ReasonContentBoundary; screenHeld != row.screenHeld {
				t.Fatalf("screen: landed=%v reason=%q, want held=%v", landed, reason, row.screenHeld)
			}
			vars := map[string]any{}
			for k, v := range row.beltVars {
				vars[k] = v
			}
			e := &ClawExecutor{logger: iterlog.Nop(), vars: vars, modelOverrides: ov}
			WithContentBoundary(&store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway})(e)
			kept := slices.ContainsFunc(e.resolveChain(node), func(el chainElement) bool { return el.Label != "" })
			if kept == row.beltHeld {
				t.Fatalf("belt: policy rung kept=%v under the override %q (vars %v), want held=%v", kept, row.override, row.beltVars, row.beltHeld)
			}
		})
	}
}
