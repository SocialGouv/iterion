package model

import (
	"context"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The route-scoped and supervisor selectors (ADR-132 §2 D21, ADR-131 §8) as
// the run's folded list carries them — bot-free: `node#route`,
// `supervisor:<name>`.

const scopedGateway = "openai_compatible/scaleway/deepseek-v4.1-flash"

// gatewayRouteRows pins a node and its `glm` route to the gateway on claw,
// the way the activation block does.
func gatewayRouteRows(node string) ModelOverrides {
	return OverridesFrom([]OverrideEntry{
		{Selector: node, Backend: "claw", Provider: "openai_compatible", Model: scopedGateway},
		{Selector: node + "#glm", Backend: "claw", Provider: "openai_compatible", Model: scopedGateway},
	})
}

// A route cell reaches exactly its route — by name, or by index when the
// author named none — never the node's own route, never a launch stage or
// a policy rung, and pins no effort.
func TestForRoute_ReachesExactlyTheAuthoredRouteItNames(t *testing.T) {
	t.Parallel()
	o := OverridesFrom([]OverrideEntry{
		{Selector: "rev#glm", Backend: "claw", Model: scopedGateway, Provider: "openai_compatible", Effort: "high"},
		{Selector: "rev#1", Model: "openai_compatible/x"},
	})
	if got := o.ForRoute("rev", 0, "glm"); got.Model != scopedGateway || got.Backend != "claw" || got.Provider != "openai_compatible" || got.Effort != "" {
		t.Fatalf("ForRoute(rev#glm) = %+v, want the cell's backend/model/provider and no effort", got)
	}
	if got := o.ForRoute("rev", 1, ""); got.Model != "openai_compatible/x" {
		t.Fatalf("ForRoute(rev#1) = %+v, want the index key to reach an unnamed route", got)
	}
	if got := o.ForRoute("other", 0, "glm"); !got.Empty() {
		t.Fatalf("ForRoute(other#glm) = %+v, want nothing", got)
	}
	if got := o.ForNode("rev", ir.NodeJudge); !got.Empty() {
		t.Fatalf("ForNode(rev) = %+v: a route cell reached the node's own route", got)
	}
	fb := ir.Fallback{Name: "glm", Model: "glm-5.3", Provider: "zai", On: []string{"usage_window"}, Metered: true}
	got := o.EffectiveFallback("rev", 0, fb)
	if got.Model != scopedGateway || got.Backend != "claw" || got.Provider != "openai_compatible" || got.Name != "glm" || !got.Metered || !slices.Equal(got.On, fb.On) {
		t.Fatalf("EffectiveFallback = %+v, want the pinned fields and the author's name, triggers and metering", got)
	}
	for _, runtimeOnly := range []ir.Fallback{
		{Name: "glm", Model: "glm-5.3", RunStageSet: true},
		{Name: "glm", Model: "glm-5.3", Policy: true},
	} {
		if got := o.EffectiveFallback("rev", 0, runtimeOnly); got.Model != "glm-5.3" {
			t.Fatalf("EffectiveFallback pinned %+v — a launch stage or a policy rung is no authored route", runtimeOnly)
		}
	}
}

// A supervisor cell pins the supervisor's model, the later rule winning a
// tie and a higher rank winning over a lower.
func TestForSupervisor_PinsTheDeclaredModel(t *testing.T) {
	t.Parallel()
	base := OverridesFrom([]OverrideEntry{{Selector: "supervisor:pacer", Model: "openai_compatible/a"}, {Selector: "supervisor:pacer", Model: "openai_compatible/b"}})
	sup := &ir.Supervisor{Name: "pacer", Model: "${PACER_DIAL:-anthropic/claude-opus-5-5}"}
	if got := base.EffectiveSupervisorModel(sup); got != "openai_compatible/b" {
		t.Fatalf("EffectiveSupervisorModel = %q, want the later rule's", got)
	}
	launch := OverridesFrom([]OverrideEntry{{Selector: "supervisor:pacer", Model: "openai_compatible/launch"}})
	if got := launch.MergeOver(base).EffectiveSupervisorModel(sup); got != "openai_compatible/launch" {
		t.Fatalf("EffectiveSupervisorModel = %q, want the higher rank's", got)
	}
	if got := base.EffectiveSupervisorModel(&ir.Supervisor{Name: "other", Model: "anthropic/x"}); got != "anthropic/x" {
		t.Fatalf("EffectiveSupervisorModel(other) = %q, want the declaration's own", got)
	}
}

// Dispatch walks the route the walk certified: a route cell replaces the
// rescue's backend, provider and model in the chain the executor builds.
func TestResolveChain_ARouteCellPinsTheRescueDispatchWalks(t *testing.T) {
	t.Parallel()
	const rescueModel = "openai_compatible/scaleway/glm-5.3"
	node := chainAgentNode("rev", delegate.BackendClaudeCode, "anthropic", []ir.Fallback{{Name: "glm", Model: "glm-5.3", Provider: "zai"}})
	e := &ClawExecutor{modelOverrides: OverridesFrom([]OverrideEntry{
		{Selector: "rev", Backend: "claw", Provider: "openai_compatible", Model: scopedGateway},
		{Selector: "rev#glm", Backend: "claw", Provider: "openai_compatible", Model: rescueModel},
	})}
	chain := e.resolveChain(node)
	i := slices.IndexFunc(chain, func(el chainElement) bool { return el.Label == "glm" })
	if i < 0 {
		t.Fatalf("chain = %+v, want the glm rescue", chain)
	}
	if el := chain[i]; el.Provider != "openai_compatible" || el.Model != rescueModel || el.Backend != "claw" {
		t.Fatalf("rescue element = %+v, want claw + openai_compatible + %s", el, rescueModel)
	}
}

// The walk rule: a cell fixing backend, provider and model decides the
// node's route from its literals — the node's own `provider:` dial, which
// the provider cell collapses at dispatch, is read nowhere: no deferral, no
// widening, the site in DecidedPrimary. Without the provider cell, the dial
// still decides the hint, and the route stays the environment's.
func TestEffectiveProviders_ACellFixingEveryFieldDecidesTheRoute(t *testing.T) {
	t.Parallel()
	// The dials are names no deployment sets: each span reads its default.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"rev": &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "rev"}, LLMFields: ir.LLMFields{
			Backend:  "${SCOPED_UNSET_BACKEND_DIAL:-claw}",
			Provider: "${SCOPED_UNSET_PROVIDER_DIAL:-}",
			Model:    "${SCOPED_UNSET_MODEL_DIAL:-openai/gpt-6-astra}",
		}},
	}}
	known := KnownProviderSet()
	full := EffectiveProviders(RouteView{Workflow: wf, Overrides: gatewayRouteRows("rev"), GatewayKeyFunded: true}, known)
	if !full.NarrowSafe || full.EnvDeferred || len(full.UnattributedSites) != 0 || len(full.DeferredSites) != 0 {
		t.Fatalf("full cell: narrow_safe=%v env_deferred=%v unattributed=%v deferred=%v — the collapsed dial was read", full.NarrowSafe, full.EnvDeferred, full.UnattributedSites, full.DeferredSites)
	}
	if !slices.Equal(full.DecidedPrimary["openai_compatible"], []string{"rev"}) {
		t.Fatalf("full cell: decided primaries = %v, want rev decided on the gateway", full.DecidedPrimary)
	}
	partial := EffectiveProviders(RouteView{Workflow: wf, Overrides: OverridesFrom([]OverrideEntry{{Selector: "rev", Backend: "claw", Model: scopedGateway}}), GatewayKeyFunded: true}, known)
	if partial.NarrowSafe || !partial.EnvDeferred {
		t.Fatalf("a cell that leaves the provider to the node's empty dial decided the route: %+v", partial)
	}
}

// The gateway hint on claw: the gate counts it honoured — claw's dispatch
// pairs it with the model (GatewayCrossingRefusal) — while the registry's
// force-Anthropic-direct switch stays the anthropic hint's alone, so a
// gateway hint never sends an Anthropic credential to the direct wire.
func TestClawHonoursTheGatewayHintWithoutForcingAnthropicDirect(t *testing.T) {
	t.Parallel()
	if !HonoursProviderHintValue(delegate.BackendClaw, "openai_compatible") {
		t.Fatal("claw/openai_compatible must count as honoured: the gateway crossing check serves it or refuses it loudly")
	}
	if clawForcesAnthropicDirect("openai_compatible") {
		t.Fatal("the gateway hint must not force Anthropic-direct")
	}
	ctx := credroute.WithEnv(context.Background(), map[string]string{"ANTHROPIC_BASE_URL": "https://facade.example"})
	if got := runCredentialBaseURL(ctx, "anthropic/claude-opus-5-5", "openai_compatible"); got != "https://facade.example" {
		t.Fatalf("runCredentialBaseURL under the gateway hint = %q, want the env's base URL (no direct-wire switch)", got)
	}
	if err := GatewayCrossingRefusal(delegate.BackendClaw, "openai_compatible", scopedGateway); err != nil {
		t.Fatalf("claw + gateway hint + gateway model refused: %v", err)
	}
	if err := GatewayCrossingRefusal(delegate.BackendClaw, "openai_compatible", "anthropic/claude-opus-5-5"); err == nil {
		t.Fatal("claw + gateway hint + vendor model served — the pin would be ignored")
	}
	if err := GatewayCrossingRefusal(delegate.BackendClaudeCode, "openai_compatible", scopedGateway); err == nil {
		t.Fatal("claude_code served a gateway route — the parity refusal is gone")
	}
}

// SelectorReaches answers the zero-match warning: each class against the
// sites it names.
func TestSelectorReaches(t *testing.T) {
	t.Parallel()
	wf := &ir.Workflow{
		Nodes: map[string]ir.Node{
			"rev":  chainAgentNode("rev", "claw", "", []ir.Fallback{{Name: "glm"}, {}}),
			"tool": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool"}},
		},
		Supervisors: []*ir.Supervisor{{Name: "pacer"}},
	}
	for sel, want := range map[string]bool{
		"rev": true, "re*": true, "agent": true, "*": true, "judge": false, "tool": false, "nope": false,
		"rev#glm": true, "rev#1": true, "rev#gml": false, "rev#0": false,
		"supervisor:pacer": true, "supervisor:pacr": false,
	} {
		if got := SelectorReaches(wf, sel); got != want {
			t.Errorf("SelectorReaches(%q) = %v, want %v", sel, got, want)
		}
	}
}
