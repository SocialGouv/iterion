package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func recoveryToolWith(id, policy string, rec *ir.RecoverySpec) *ir.ToolNode {
	return &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: id},
		Command:       "make build",
		Goal:          "the build succeeds",
		Postcondition: "test -f build.ok",
		Policy:        policy,
		Recovery:      rec,
	}
}

// A tool node's `recovery: model:` is a route the walk reads the way
// dispatch spends it (#2783): rung 3 as a direct generation (no backend, no
// override cell), rung 4 as the synthetic agent dispatch — and the empty
// spec through the ladder's own default. Mutant: the recovery arm dropped
// from EffectiveProviders → every assertion below reddens (the rung's
// provider vanishes from Providers and Pins, the unreadable entry with it).
func TestEffectiveProviders_RecoveryRungsAreReadAsDispatchSpendsThem(t *testing.T) {
	t.Setenv("ITERION_VERIFIED_ACTION_MODEL", "")
	view := func(tool *ir.ToolNode, ov ModelOverrides, allowed []string) RouteView {
		wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": agentWith("a", "zai", ""), "t": tool}}
		return RouteView{Workflow: wf, Overrides: ov, AllowedProviders: allowed}
	}
	repair := &ir.RecoverySpec{MaxRepairAttempts: 1, Model: "openai/gpt-5.4-mini"}
	agent := &ir.RecoverySpec{MaxAgentAttempts: 1, Model: "openai/gpt-5.4-mini"}

	// Rung 3 alone: the direct generation, at its own site.
	got := EffectiveProviders(view(recoveryToolWith("t", ir.PolicyRecover, repair), ModelOverrides{}, nil), knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"openai", "zai"}) || !slicesEqual(got.Pins["openai"], []string{"t#self_repair"}) {
		t.Fatalf("rung 3 on openai/…: got %+v, want [openai zai] narrow-safe, pinned at t#self_repair", got)
	}
	// Rung 4 alone: the synthetic agent dispatch, at its own site.
	got = EffectiveProviders(view(recoveryToolWith("t", ir.PolicyRecover, agent), ModelOverrides{}, nil), knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Pins["openai"], []string{"t#agent_recovery"}) {
		t.Fatalf("rung 4 on openai/…: got %+v, want the pin at t#agent_recovery", got)
	}
	// Both rungs name their own site for the one spec.
	both := &ir.RecoverySpec{MaxRepairAttempts: 1, MaxAgentAttempts: 1, Model: "openai/gpt-5.4-mini"}
	got = EffectiveProviders(view(recoveryToolWith("t", ir.PolicyRecover, both), ModelOverrides{}, nil), knownForTest)
	if !slicesEqual(got.Pins["openai"], []string{"t#agent_recovery", "t#self_repair"}) {
		t.Fatalf("both rungs: got pins %+v, want [t#agent_recovery t#self_repair]", got.Pins)
	}
	// An unnamed spec resolves through the ladder's default arm, exactly as
	// recoveryModel resolves it at dispatch: the package default is an
	// anthropic route, and it is refused by name under a list without it.
	unnamed := &ir.RecoverySpec{MaxRepairAttempts: 1}
	got = EffectiveProviders(view(recoveryToolWith("t", ir.PolicyRecover, unnamed), ModelOverrides{}, nil), knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"anthropic", "zai"}) || !slicesEqual(got.Pins["anthropic"], []string{"t#self_repair"}) {
		t.Fatalf("unnamed spec: got %+v, want the anthropic default pinned at t#self_repair", got)
	}
	got = EffectiveProviders(view(recoveryToolWith("t", ir.PolicyRecover, unnamed), ModelOverrides{}, []string{"zai"}), knownForTest)
	if !slicesEqual(got.ForbiddenPrimary["anthropic"], []string{"t#self_repair"}) {
		t.Fatalf("the default outside the list: got %+v, want ForbiddenPrimary[anthropic]=[t#self_repair]", got.Forbidden)
	}
	// … and ITERION_VERIFIED_ACTION_MODEL is that ladder's middle rung.
	t.Setenv("ITERION_VERIFIED_ACTION_MODEL", "openai/gpt-5.4-mini")
	got = EffectiveProviders(view(recoveryToolWith("t", ir.PolicyRecover, unnamed), ModelOverrides{}, nil), knownForTest)
	if !slicesEqual(got.Providers, []string{"openai", "zai"}) {
		t.Fatalf("env-dialled default: got %+v, want [openai zai]", got)
	}
	t.Setenv("ITERION_VERIFIED_ACTION_MODEL", "")

	// An inert recovery block (policy != recover — C106's dead config)
	// spends nothing and reads nothing: no provider, no widening.
	inert := recoveryToolWith("t", ir.PolicyRequired, repair)
	got = EffectiveProviders(view(inert, ModelOverrides{}, nil), knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"zai"}) {
		t.Fatalf("inert recovery (policy: required): got %+v, want [zai] narrow-safe", got)
	}

	// A spec the run's vars decide late stays written and widens — under an
	// allowlist it is refused, naming the rung's site.
	late := &ir.RecoverySpec{MaxRepairAttempts: 1, Model: "{{vars.m}}"}
	got = EffectiveProviders(view(recoveryToolWith("t", ir.PolicyRecover, late), ModelOverrides{}, nil), knownForTest)
	if got.NarrowSafe {
		t.Fatalf("a late spec must widen: %+v", got)
	}
	got = EffectiveProviders(view(recoveryToolWith("t", ir.PolicyRecover, late), ModelOverrides{}, []string{"openai", "zai"}), knownForTest)
	if len(got.Unreadable) == 0 || got.Unreadable[0].Site != "t#self_repair" {
		t.Fatalf("a late spec under a list: got %+v, want an unreadable entry at t#self_repair", got.Unreadable)
	}

	// Under an allowlist the rung is a PRIMARY route: outside the list it is
	// refused naming the node; inside, it is served.
	got = EffectiveProviders(view(recoveryToolWith("t", ir.PolicyRecover, repair), ModelOverrides{}, []string{"zai"}), knownForTest)
	if !slicesEqual(got.ForbiddenPrimary["openai"], []string{"t#self_repair"}) || len(got.Unreadable) != 0 {
		t.Fatalf("rung 3 outside the list: got %+v, want ForbiddenPrimary[openai]=[t#self_repair] and nothing unreadable", got)
	}
	got = EffectiveProviders(view(recoveryToolWith("t", ir.PolicyRecover, repair), ModelOverrides{}, []string{"openai", "zai"}), knownForTest)
	if len(got.ForbiddenPrimary) != 0 || len(got.Unreadable) != 0 || !slicesEqual(got.Pins["openai"], []string{"t#self_repair"}) {
		t.Fatalf("rung 3 inside the list: got %+v, want it served and pinned at t#self_repair", got)
	}
}

// Rung 4 is dispatched as a synthetic agent node whose id is the tool's own
// + "__recover" (agentRecovery), so a launch override reaches it through a
// KIND or GLOB selector — never through the tool node's exact id, which
// matches nothing at dispatch either. The walk reads the same cell.
// Mutant: the rung-4 arm reading the TOOL node's override cell instead → the
// kind-selector assertion reddens and the exact-selector one inverts.
func TestEffectiveProviders_RecoveryAgentRungReadsTheSyntheticNodesOverrideCell(t *testing.T) {
	t.Setenv("ITERION_VERIFIED_ACTION_MODEL", "")
	agent := &ir.RecoverySpec{MaxAgentAttempts: 1, Model: "anthropic/claude-opus-5-5"}
	view := func(ov ModelOverrides) RouteView {
		wf := &ir.Workflow{Nodes: map[string]ir.Node{"t": recoveryToolWith("t", ir.PolicyRecover, agent)}}
		return RouteView{Workflow: wf, Overrides: ov}
	}
	var kindOv ModelOverrides
	kindOv.SetProvider("agent", "openai")
	got := EffectiveProviders(view(kindOv), knownForTest)
	if !slicesEqual(got.Providers, []string{"openai"}) || !slicesEqual(got.Pins["openai"], []string{"t#agent_recovery"}) {
		t.Fatalf("a kind-agent provider override must reach the synthetic rung: got %+v, want [openai] at t#agent_recovery", got)
	}
	var exactOv ModelOverrides
	exactOv.SetProvider("t", "openai")
	got = EffectiveProviders(view(exactOv), knownForTest)
	if !slicesEqual(got.Providers, []string{"anthropic"}) || !slicesEqual(got.Pins["anthropic"], []string{"t#agent_recovery"}) {
		t.Fatalf("the tool node's exact selector must NOT reach the rung (dispatch's id differs): got %+v, want [anthropic]", got)
	}
}

// The wire guard reads the same rungs (#2783): a recovery model on an
// anthropic-wire prefix is a readable route on the wire, one off it is no
// route, and a spec the walk cannot read keeps the guard armed. Mutant: the
// recovery arm dropped from AnthropicWireRoutes → the on-wire rung
// disappears and the off-wire one reappears as unreadable (rung 4's old
// fieldless reading).
func TestAnthropicWireRoutesReadsRecoveryRungs(t *testing.T) {
	t.Setenv("ITERION_VERIFIED_ACTION_MODEL", "")
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"rep-on":     recoveryToolWith("rep-on", ir.PolicyRecover, &ir.RecoverySpec{MaxRepairAttempts: 1, Model: "anthropic/claude-opus-5-5"}),
		"rep-off":    recoveryToolWith("rep-off", ir.PolicyRecover, &ir.RecoverySpec{MaxRepairAttempts: 1, Model: "openai/gpt-5.4-mini"}),
		"rep-var":    recoveryToolWith("rep-var", ir.PolicyRecover, &ir.RecoverySpec{MaxRepairAttempts: 1, Model: "{{vars.m}}"}),
		"rep-def":    recoveryToolWith("rep-def", ir.PolicyRecover, &ir.RecoverySpec{MaxRepairAttempts: 1}),
		"rep-inert":  recoveryToolWith("rep-inert", ir.PolicyRequired, &ir.RecoverySpec{MaxRepairAttempts: 1, Model: "anthropic/claude-opus-5-5"}),
		"rep-agent4": recoveryToolWith("rep-agent4", ir.PolicyRecover, &ir.RecoverySpec{MaxAgentAttempts: 1, Model: "openai/gpt-5.4-mini"}),
	}}
	got := AnthropicWireRoutes(RouteView{Workflow: wf, Overrides: ModelOverrides{}})
	want := []WireRoute{
		{NodeID: "rep-agent4#agent_recovery", Backend: "", Hint: "", Model: "openai/gpt-5.4-mini", Readable: false},
		{NodeID: "rep-def#self_repair", Backend: "", Hint: "anthropic", Model: "anthropic/claude-opus-5-5", Readable: true},
		{NodeID: "rep-on#self_repair", Backend: "", Hint: "anthropic", Model: "anthropic/claude-opus-5-5", Readable: true},
		{NodeID: "rep-var#self_repair", Backend: "", Hint: "", Model: "{{vars.m}}", Readable: false},
	}
	if len(got) != len(want) {
		t.Fatalf("routes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("route %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Rung 4 off the wire when the backend and the model both say so: the
	// guard must NOT arm for an openai recovery agent on claw.
	wfClaw := &ir.Workflow{
		DefaultBackend: "claw",
		Nodes: map[string]ir.Node{
			"rep-agent4": recoveryToolWith("rep-agent4", ir.PolicyRecover, &ir.RecoverySpec{MaxAgentAttempts: 1, Model: "openai/gpt-5.4-mini"}),
		},
	}
	if got := AnthropicWireRoutes(RouteView{Workflow: wfClaw, Overrides: ModelOverrides{}}); len(got) != 0 {
		t.Fatalf("claw/openai rung 4 must not arm the wire guard: %+v", got)
	}
}
