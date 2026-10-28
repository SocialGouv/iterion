package model

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
)

func humanWith(id string, interaction ir.InteractionMode, mdl string) *ir.HumanNode {
	return &ir.HumanNode{
		BaseNode:          ir.BaseNode{ID: id},
		InteractionFields: ir.InteractionFields{Interaction: interaction},
		Model:             mdl,
	}
}

// A human node's LLM half is a route the walk reads the way dispatch spends
// it (#2691): a direct generation on node.Model — its prefix names the
// provider, no backend, and no override cell applies (executeHumanLLMWithSpend
// never consults ForNode). Mutant: the human arm dropped → the node is read
// as fieldless and the narrow-safe assertions redden.
func TestEffectiveProviders_HumanNodeModelIsARoute(t *testing.T) {
	view := func(h *ir.HumanNode, ov ModelOverrides, allowed []string) RouteView {
		wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": agentWith("a", "zai", ""), "h": h}}
		return RouteView{Workflow: wf, Overrides: ov, AllowedProviders: allowed}
	}
	got := EffectiveProviders(view(humanWith("h", ir.InteractionLLM, "openai/gpt-5"), ModelOverrides{}, nil), knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"openai", "zai"}) {
		t.Fatalf("human llm on openai/…: got %+v, want [openai zai] narrow-safe", got)
	}
	got = EffectiveProviders(view(humanWith("h", ir.InteractionLLMOrHuman, "anthropic/claude-opus-4-8"), ModelOverrides{}, nil), knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"anthropic", "zai"}) {
		t.Fatalf("human llm_or_human on anthropic/…: got %+v, want [anthropic zai] narrow-safe", got)
	}
	// A human node that only pauses spends nothing on its model.
	got = EffectiveProviders(view(humanWith("h", ir.InteractionHuman, "openai/gpt-5"), ModelOverrides{}, nil), knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"zai"}) {
		t.Fatalf("human-only node: got %+v, want [zai]", got)
	}
	// A bare spec widens, exactly as an agent's interaction_model does.
	if got = EffectiveProviders(view(humanWith("h", ir.InteractionLLM, ""), ModelOverrides{}, nil), knownForTest); got.NarrowSafe {
		t.Fatalf("human llm on a bare spec must widen: %+v", got)
	}
	// Dispatch reads node.Model and no override cell: a launch override on
	// the node's selector does not re-target the human half.
	var ov ModelOverrides
	ov.SetModel("h", "anthropic/claude-opus-4-8")
	got = EffectiveProviders(view(humanWith("h", ir.InteractionLLM, "openai/gpt-5"), ov, nil), knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"openai", "zai"}) {
		t.Fatalf("an override must not reach the human half: got %+v, want [openai zai]", got)
	}
	// An env-dialled spec with a default reads its default's prefix.
	t.Setenv("HUMAN_ROUTE_TEST_MODEL", "")
	got = EffectiveProviders(view(humanWith("h", ir.InteractionLLM, "${HUMAN_ROUTE_TEST_MODEL:-anthropic/claude-opus-4-8}"), ModelOverrides{}, nil), knownForTest)
	if !slicesEqual(got.Providers, []string{"anthropic", "zai"}) || len(got.Unreadable) != 0 {
		t.Fatalf("env-dialled human model: got %+v, want [anthropic zai] and nothing unreadable", got)
	}
	// Under an allowlist the human half is a PRIMARY route: outside the
	// list it is refused naming the node; inside, it is served.
	got = EffectiveProviders(view(humanWith("h", ir.InteractionLLM, "openai/gpt-5"), ModelOverrides{}, []string{"zai"}), knownForTest)
	if !slicesEqual(got.ForbiddenPrimary["openai"], []string{"h"}) || len(got.Unreadable) != 0 {
		t.Fatalf("human llm outside the list: got %+v, want ForbiddenPrimary[openai]=[h] and nothing unreadable", got)
	}
	got = EffectiveProviders(view(humanWith("h", ir.InteractionLLM, "openai/gpt-5"), ModelOverrides{}, []string{"openai", "zai"}), knownForTest)
	if len(got.ForbiddenPrimary) != 0 || len(got.Unreadable) != 0 || !slicesEqual(got.Pins["openai"], []string{"h"}) {
		t.Fatalf("human llm inside the list: got %+v, want it served and pinned at h", got)
	}
}

// The wire guard reads the same route: a human half on an anthropic-wire
// prefix is a readable route on the wire, one off it is no route, and a
// prefix the walk cannot read keeps the guard armed. Mutant: the human arm
// dropped → every LLM-mode human node yields an unreadable route and the
// off-wire node appears.
func TestAnthropicWireRoutesReadsAHumanNodesDirectRoute(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"h-anthropic": humanWith("h-anthropic", ir.InteractionLLM, "anthropic/claude-opus-5-5"),
		"h-openai":    humanWith("h-openai", ir.InteractionLLMOrHuman, "openai/gpt-5"),
		"h-bare":      humanWith("h-bare", ir.InteractionLLM, ""),
		"h-var":       humanWith("h-var", ir.InteractionLLM, "{{vars.m}}"),
		"h-pause":     humanWith("h-pause", ir.InteractionHuman, "anthropic/claude-opus-5-5"),
	}}
	got := AnthropicWireRoutes(RouteView{Workflow: wf, Overrides: ModelOverrides{}})
	want := []WireRoute{
		{NodeID: "h-anthropic", Backend: "", Hint: "anthropic", Model: "anthropic/claude-opus-5-5", Readable: true},
		{NodeID: "h-bare", Backend: "", Hint: "", Model: "", Readable: false},
		{NodeID: "h-var", Backend: "", Hint: "", Model: "{{vars.m}}", Readable: false},
	}
	if len(got) != len(want) {
		t.Fatalf("routes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("route %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The ticket's witness on the real bundle (#2691): dep-update-guard's
// `escalate` pins `anthropic/…` on its llm_or_human half. Under
// `providers: [anthropic]` the node is served and pinned; under a list
// without anthropic it is refused NAMING the node. Mutant: the human arm
// dropped → escalate is read as fieldless and the served case reddens on
// the unreadable entry.
func TestEffectiveProviders_DepUpdateGuardEscalateIsReadAsDispatchSpendsIt(t *testing.T) {
	src, err := os.ReadFile("../../../bots/dep-update-guard/main.bot")
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	t.Setenv("VETTY_MODEL_ESCALATE", "")
	pr := parsertest.Parse("main.bot", string(src))
	res := ir.Compile(pr.File)
	if res.HasErrors() {
		t.Fatalf("compile: %+v", res.Diagnostics)
	}
	h, ok := res.Workflow.Nodes["escalate"].(*ir.HumanNode)
	if !ok || !strings.HasPrefix(ir.ExpandEnvWithDefault(h.Model), "anthropic/") {
		t.Fatalf("fixture drift: escalate = %T %q, want a human node whose model defaults to anthropic/…", res.Workflow.Nodes["escalate"], h.Model)
	}
	served := EffectiveProviders(RouteView{Workflow: res.Workflow, Overrides: ModelOverrides{}, AllowedProviders: []string{"anthropic"}}, knownForTest)
	for _, u := range served.Unreadable {
		if u.Site == "escalate" {
			t.Fatalf("escalate is read as unreadable under [anthropic]: %+v", u)
		}
	}
	for p, sites := range served.ForbiddenPrimary {
		for _, s := range sites {
			if s == "escalate" {
				t.Fatalf("escalate forbidden on %s under [anthropic]: %+v", p, served.ForbiddenPrimary)
			}
		}
	}
	if !containsString(served.Pins["anthropic"], "escalate") {
		t.Fatalf("escalate not pinned on anthropic: pins = %+v", served.Pins)
	}
	refused := EffectiveProviders(RouteView{Workflow: res.Workflow, Overrides: ModelOverrides{}, AllowedProviders: []string{"openai"}}, knownForTest)
	if !containsString(refused.ForbiddenPrimary["anthropic"], "escalate") {
		t.Fatalf("escalate not refused by name under [openai]: forbidden = %+v", refused.ForbiddenPrimary)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
