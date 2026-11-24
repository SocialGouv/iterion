package model

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

const (
	walkGatewayStep = "openai_compatible:openai_compatible/team/m"
	walkVendorModel = "anthropic/claude-opus-5"
)

// walkStepOrigin builds a run whose node "a" carries a claw route with a
// vendor model beside a `provider:` step the origin writes.
type walkStepOrigin struct {
	name string
	view func(step string) RouteView
}

func walkStepOrigins() []walkStepOrigin {
	return []walkStepOrigin{
		{name: "bot DSL", view: func(step string) RouteView {
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": gatewayNode("claw", step, walkVendorModel)}}
			return RouteView{Workflow: wf}
		}},
		{name: "node override cell", view: func(step string) RouteView {
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": gatewayNode("claw", "", walkVendorModel)}}
			var ov ModelOverrides
			ov.SetBackend("a", "claw")
			ov.SetProvider("a", step)
			return RouteView{Workflow: wf, Overrides: ov}
		}},
		{name: "trusted launch var", view: func(step string) RouteView {
			wf := &ir.Workflow{
				Vars:  map[string]*ir.Var{"p": {Name: "p", Type: ir.VarString}},
				Nodes: map[string]ir.Node{"a": gatewayNode("claw", "{{vars.p}}", walkVendorModel)},
			}
			return RouteViewForLaunch(wf, ModelOverrides{}, nil, map[string]any{"p": step}, []string{"p"})
		}},
	}
}

// A `provider:` step written `hint:model` is dispatched on its own model
// (parseProviderChain), so the credential walk judges it there: a gateway
// step beside a vendor model is the gateway route — keyed, the
// openai_compatible provider pinned at its site; unkeyed, env-funded — and an
// allowlist naming the gateway admits it. Judged on the vendor model instead,
// the step widens as an unknown name, the site is pinned to the vendor the
// step never spends, and the allowlist refuses a launch dispatch serves.
// Mutant: chainSteps drops the step model (`hint, _, _ :=`) → every origin
// row reds on its keyed, unkeyed and allowlist assertions.
func TestEffectiveProviders_aGatewayStepBesideAVendorModelIsJudgedOnItsOwnModel(t *testing.T) {
	t.Parallel()
	gw := "openai_compatible"
	for _, origin := range walkStepOrigins() {
		t.Run(origin.name, func(t *testing.T) {
			keyedView := origin.view(walkGatewayStep)
			keyedView.GatewayKeyFunded = true
			keyed := EffectiveProviders(keyedView, KnownProviderSet())
			if !slices.Equal(keyed.Providers, []string{gw}) || !keyed.NarrowSafe || len(keyed.Unknown) != 0 {
				t.Errorf("keyed: providers=%v narrow=%v unknown=%v, want [%s] narrow-safe with no unknown name", keyed.Providers, keyed.NarrowSafe, keyed.Unknown, gw)
			}
			if !slices.Equal(keyed.Sites["a"], []string{gw}) {
				t.Errorf("keyed: sites[a]=%v, want [%s] — the step spends the gateway, never the vendor model beside it", keyed.Sites["a"], gw)
			}

			unkeyed := EffectiveProviders(origin.view(walkGatewayStep), KnownProviderSet())
			if len(unkeyed.Unknown) != 0 || !unkeyed.NarrowSafe || !unkeyed.OnlyEnvFunded() || len(unkeyed.Providers) != 0 {
				t.Errorf("unkeyed: providers=%v narrow=%v unknown=%v onlyEnvFunded=%v, want an env-funded gateway route", unkeyed.Providers, unkeyed.NarrowSafe, unkeyed.Unknown, unkeyed.OnlyEnvFunded())
			}

			listedView := origin.view(walkGatewayStep)
			listedView.AllowedProviders = []string{gw}
			if err := AllowlistRefusal(EffectiveProviders(listedView, KnownProviderSet()), "test"); err != nil {
				t.Errorf("allowlist [%s]: refused a launch dispatch serves on the gateway alone: %v", gw, err)
			}
		})
	}
}

// claw's slot follows the model's prefix (prefixSlotForClaw), and a step's
// own model is the one claw sends: a vendor step `anthropic:zai/glm-5.3`
// records the z.ai slot beside its hint, pinned at its site.
// Mutant: chainSteps drops the step model → Sites[a] reads [anthropic].
func TestEffectiveProviders_aClawVendorStepNamesItsOwnPrefixSlot(t *testing.T) {
	t.Parallel()
	for _, origin := range walkStepOrigins() {
		t.Run(origin.name, func(t *testing.T) {
			got := EffectiveProviders(origin.view("anthropic:zai/glm-5.3"), KnownProviderSet())
			if !slices.Equal(got.Sites["a"], []string{"anthropic", "zai"}) {
				t.Errorf("sites[a]=%v, want [anthropic zai] — the step's own prefix slot", got.Sites["a"])
			}
			if !slices.Contains(got.Pins["zai"], "a") {
				t.Errorf("pins[zai]=%v, want the site a", got.Pins["zai"])
			}
			if !got.NarrowSafe {
				t.Errorf("narrow=false (unknown=%v), want narrow-safe", got.Unknown)
			}
		})
	}
}

// The readings a step model does not move: a chain whose steps carry no
// model is judged on the route model as before, hints first then the
// route's prefix slot; a step model inside a `${…}` field is this process's
// expansion, never a raw gateway spelling, so the gateway hint stays an
// unknown name; and a step model applies to its own step only.
// Mutants: the `${…}` guard dropped → "env-templated field" reds; the route
// model ignored for model-less steps → "mixed chain" and "route model" red.
func TestEffectiveProviders_stepModelControls(t *testing.T) {
	t.Parallel()
	gw := "openai_compatible"
	rows := []struct {
		name        string
		provider    string
		model       string
		keyed       bool
		wantSites   []string
		wantUnknown []string
		wantNarrow  bool
	}{
		{name: "gateway hint on the gateway route model", provider: gw, model: "openai_compatible/team/m", keyed: true, wantSites: []string{gw}, wantNarrow: true},
		{name: "vendor hint, no step model: hint then route prefix", provider: "anthropic", model: "openai/gpt-5", wantSites: []string{"anthropic", "openai"}, wantNarrow: true},
		{name: "gateway hint on a vendor route model stays unknown", provider: gw, model: walkVendorModel, keyed: true, wantSites: []string{"anthropic"}, wantUnknown: []string{gw}},
		{name: "env-templated field: the step model is not a raw gateway spelling", provider: "${QW3213_UNSET_STEP:-" + walkGatewayStep + "}", model: walkVendorModel, keyed: true, wantUnknown: []string{gw}},
		{name: "mixed chain: the model-less step reads the route model", provider: walkGatewayStep + ",anthropic", model: "openai/gpt-5", keyed: true, wantSites: []string{gw, "anthropic", "openai"}, wantNarrow: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": gatewayNode("claw", row.provider, row.model)}}
			got := EffectiveProviders(RouteView{Workflow: wf, GatewayKeyFunded: row.keyed}, KnownProviderSet())
			if !slices.Equal(got.Sites["a"], row.wantSites) {
				t.Errorf("sites[a]=%v, want %v", got.Sites["a"], row.wantSites)
			}
			if !slices.Equal(got.Unknown, row.wantUnknown) {
				t.Errorf("unknown=%v, want %v", got.Unknown, row.wantUnknown)
			}
			if got.NarrowSafe != row.wantNarrow {
				t.Errorf("narrow=%v, want %v", got.NarrowSafe, row.wantNarrow)
			}
		})
	}
}

// Dispatch never splits a rescue route's `provider:` field: resolveChainUnder
// sends an authored fallback, or a run-level stage, with the field whole as
// its provider and the route's own model. The walk judges the route on that
// model, so claw's slot is the route model's prefix — never a `hint:model`
// step model dispatch does not send.
// Mutant: resolveRoute passes stepModels=true → both rows read [zai] alone.
func TestEffectiveProviders_aRescueRouteIsJudgedOnItsRouteModel(t *testing.T) {
	t.Parallel()
	primary := ir.LLMFields{Backend: "claw", Provider: "anthropic", Model: walkVendorModel}
	authored := &ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: primary,
		Fallbacks: []ir.Fallback{{Name: "r", Backend: "claw", Provider: "zai:zai/glm-5.3", Model: "openai/gpt-5"}},
	}}}
	runLevel := &ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: primary}}}
	rows := []struct {
		name string
		view RouteView
		site string
	}{
		{name: "authored fallbacks route", view: RouteView{Workflow: authored}, site: "a#r"},
		{name: "run-level chain stage", view: RouteView{Workflow: runLevel, RunFallbacks: []FallbackEntry{{Backend: "claw", Provider: "zai:zai/glm-5.3", Model: "openai/gpt-5"}}}, site: ir.RunFallbackName + "#0"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := EffectiveProviders(row.view, KnownProviderSet())
			if !slices.Contains(got.Sites[row.site], "openai") {
				t.Errorf("Sites[%s]=%v lacks openai — the route model's prefix slot claw spends", row.site, got.Sites[row.site])
			}
			if !slices.Contains(got.Providers, "openai") {
				t.Errorf("providers=%v lacks openai", got.Providers)
			}
		})
	}
}

// A hinted step the allowlist forbids is dropped by dispatch on its hint
// alone (AllowlistDrops), its model never sent: its claw slot read must not
// mark the route servable inside the list, or the launch is admitted and
// the node refused mid-run with no leg.
// Mutant: the slot loop reads a forbidden step's model → both rows admitted.
func TestAllowlistRefusal_aForbiddenHintedClawStepIsRefused(t *testing.T) {
	t.Parallel()
	rows := []struct{ name, provider, model string }{
		{name: "step model inside the list", provider: "anthropic:zai/glm-5.3", model: walkVendorModel},
		{name: "route model inside the list", provider: "anthropic", model: "zai/glm-5.3"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": gatewayNode("claw", row.provider, row.model)}}
			view := RouteView{Workflow: wf, AllowedProviders: []string{"zai"}}
			walk := EffectiveProviders(view, KnownProviderSet())
			if legs := Legs(view, wf.Nodes["a"]); len(legs) != 0 {
				t.Fatalf("legs=%v, want none — dispatch drops the forbidden hinted step", legs)
			}
			if err := AllowlistRefusal(walk, "test"); err == nil {
				t.Errorf("admitted; providers=%v forbidden=%v — dispatch serves no leg", walk.Providers, walk.ForbiddenPrimary)
			}
		})
	}
}
