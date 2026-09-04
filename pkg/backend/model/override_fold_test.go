package model

import (
	"sort"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The pool's wants derivation and any other "what does this run target?"
// site go through EffectiveProviders. These tests pin the semantics the
// round-1 A/B probes covered on the real catalog bots:
//
//   - provider chain forms (`"zai,anthropic"`, `"zai:glm-5.2,anthropic:..."`)
//   - env expansion (`${RESCUE_PROVIDER:-zai}` — secured-renovacy's 13 nodes)
//   - explicit "auto" tokens and empty chains (sec-audit-source's
//     `${ITERION_SEC_AUDIT_PROVIDER_CHAIN:-}`)
//   - unknown provider names (widen AND name the pin, never narrow)
//   - MIXED runs (an unpinned node next to a pinned peer)
//   - node `fallbacks:` blocks and the run-level fallback chain
//   - model-calling nodes that expose no LLMFields (widen)

var knownForTest = map[string]bool{
	"anthropic":  true,
	"openai":     true,
	"bedrock":    true,
	"vertex":     true,
	"azure":      true,
	"openrouter": true,
	"xai":        true,
	"zai":        true,
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func agentWith(id, provider, mdl string) *ir.AgentNode {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: id},
		LLMFields: ir.LLMFields{Provider: provider, Model: mdl},
	}
}

func TestEffectiveProviders_ProviderChainForms(t *testing.T) {
	cases := []struct {
		name        string
		provider    string
		model       string
		wantProvs   []string
		wantNarrow  bool
		wantUnknown []string
	}{
		{"comma-separated hints", "zai,anthropic", "", []string{"anthropic", "zai"}, true, nil},
		{"provider:model steps", "zai:glm-5.2,anthropic:claude-opus-5", "", []string{"anthropic", "zai"}, true, nil},
		{"model prefix only", "", "openai/gpt-5", []string{"openai"}, true, nil},
		{"explicit auto widens", "auto", "", nil, false, nil},
		{"empty widens", "", "", nil, false, nil},
		{"unknown provider widens and is named", "fake-provider", "", nil, false, []string{"fake-provider"}},
		{"unknown model prefix widens and is named", "", "fake-provider/gpt-x", nil, false, []string{"fake-provider"}},
		// A chain mixing a known name with an unknown one still lists the
		// known name (for the log line) but widens: narrowing would drop
		// the donations the unknown half could have taken.
		{"mixed known + unknown widens", "anthropic,unknown-vendor", "", []string{"anthropic"}, false, []string{"unknown-vendor"}},
		{"auto step inside a chain widens", "anthropic,auto", "", []string{"anthropic"}, false, nil},
		{"stray commas are not steps", " zai , , anthropic ,", "", []string{"anthropic", "zai"}, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": agentWith("a", tc.provider, tc.model)}}
			got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest)
			if got.NarrowSafe != tc.wantNarrow {
				t.Errorf("NarrowSafe = %v, want %v", got.NarrowSafe, tc.wantNarrow)
			}
			if want := sortedCopy(tc.wantProvs); !slicesEqual(got.Providers, want) {
				t.Errorf("Providers = %v, want %v", got.Providers, want)
			}
			if want := sortedCopy(tc.wantUnknown); !slicesEqual(got.Unknown, want) {
				t.Errorf("Unknown = %v, want %v", got.Unknown, want)
			}
		})
	}
}

func TestEffectiveProviders_EnvExpansion(t *testing.T) {
	// The executor's resolveProviderChain applies ir.ExpandEnvWithDefault;
	// the walk MUST do the same, so `provider: "${RESCUE_PROVIDER:-zai}"`
	// with the var unset resolves to zai — the shape secured-renovacy
	// ships on 13 nodes. Read verbatim it is an unknown name.
	t.Setenv("RESCUE_PROVIDER", "")
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": agentWith("a", "${RESCUE_PROVIDER:-zai}", "")}}
	got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest)
	if !got.NarrowSafe {
		t.Fatalf("NarrowSafe = false, want true — a ${VAR:-default} chain resolves to its default; got %+v", got)
	}
	if !slicesEqual(got.Providers, []string{"zai"}) {
		t.Fatalf("Providers = %v, want [zai]", got.Providers)
	}

	// The env may supply the whole chain.
	t.Setenv("PROVIDERS", "anthropic,openai")
	wf = &ir.Workflow{Nodes: map[string]ir.Node{"a": agentWith("a", "${PROVIDERS}", "")}}
	got = EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"anthropic", "openai"}) {
		t.Fatalf("env-supplied chain: got %+v, want [anthropic openai] narrow-safe", got)
	}

	// An env ref with no default and no value is an EMPTY chain: the node
	// takes whatever the process holds, so the walk widens.
	t.Setenv("ITERION_SEC_AUDIT_PROVIDER_CHAIN", "")
	wf = &ir.Workflow{Nodes: map[string]ir.Node{"a": agentWith("a", "${ITERION_SEC_AUDIT_PROVIDER_CHAIN:-}", "")}}
	got = EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest)
	if got.NarrowSafe || len(got.Providers) != 0 || len(got.Unknown) != 0 {
		t.Fatalf("empty-by-default chain must widen with nothing named: got %+v", got)
	}
}

func TestEffectiveProviders_MixedResolvedAndUnresolvedWidens(t *testing.T) {
	// The ADR-091 shape: one openai-pinned peer next to an unpinned main
	// implementer. The pre-fix walk narrowed wants to {openai}, silently
	// dropping the implementer's claude_code forfait. Widen instead.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"implementer": agentWith("implementer", "", ""),
		"reviewer":    agentWith("reviewer", "openai", ""),
	}}
	got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest)
	if got.NarrowSafe {
		t.Fatalf("NarrowSafe = true — a mixed unpinned+pinned run must widen; got %+v", got)
	}
	if !slicesEqual(got.Providers, []string{"openai"}) {
		t.Errorf("Providers = %v, want [openai] (still listed for the log line)", got.Providers)
	}
}

func TestEffectiveProviders_HonoursOverrides(t *testing.T) {
	// A launch override collapses the resolution exactly like the
	// executor's resolveProviderChain: the provider override wins over
	// the DSL model's prefix, on a judge-kind selector too (#668).
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"j": &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, LLMFields: ir.LLMFields{Model: "anthropic/claude-opus-5"}},
	}}
	var overrides ModelOverrides
	overrides.SetProvider("judge", "openai")
	overrides.SetModel("judge", "openai/gpt-5")
	got := EffectiveProviders(wf, overrides, nil, knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"openai"}) {
		t.Fatalf("got %+v, want [openai] narrow-safe", got)
	}

	// A model-only override carries its prefix too.
	var modelOnly ModelOverrides
	modelOnly.SetModel("j", "openai/gpt-5")
	got = EffectiveProviders(wf, modelOnly, nil, knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"openai"}) {
		t.Fatalf("model-only override: got %+v, want [openai] narrow-safe", got)
	}
}

func TestEffectiveProviders_NodeFallbacksContribute(t *testing.T) {
	// G3: a `fallbacks:` route names a provider the run may spend; a pool
	// granting only the primary's provider would leave it unreachable.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a": &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "a"},
			LLMFields: ir.LLMFields{Provider: "openai"},
			Fallbacks: []ir.Fallback{
				{Name: "rescue", Backend: "claw", Model: "anthropic/claude-opus-5", Provider: "anthropic"},
				// A skip route executes nothing — it must not widen.
				{Name: "give-up", Action: ir.FallbackActionSkip},
			},
		},
	}}
	got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest)
	if !got.NarrowSafe {
		t.Fatalf("NarrowSafe = false, want true (primary, rescue resolve; skip is inert): %+v", got)
	}
	if !slicesEqual(got.Providers, []string{"anthropic", "openai"}) {
		t.Fatalf("Providers = %v, want [anthropic openai]", got.Providers)
	}

	// A route that pins nothing inherits whatever the process holds —
	// the walk cannot name it, so it widens.
	wf.Nodes["a"].(*ir.AgentNode).Fallbacks = []ir.Fallback{{Name: "blind", Backend: "claude_code"}}
	got = EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest)
	if got.NarrowSafe {
		t.Fatalf("a fallback route with no provider must widen: %+v", got)
	}
}

func TestEffectiveProviders_RunLevelFallbackChain(t *testing.T) {
	// G3: --fallback / spec.Fallback / prior.Fallback add providers the
	// same way an authored route does.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": agentWith("a", "openai", "")}}
	got := EffectiveProviders(wf, ModelOverrides{}, []FallbackEntry{{Backend: "claw", Provider: "anthropic"}}, knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"anthropic", "openai"}) {
		t.Fatalf("got %+v, want [anthropic openai] narrow-safe", got)
	}
	// The empty stage ApplyRunFallback drops is dropped here too.
	got = EffectiveProviders(wf, ModelOverrides{}, []FallbackEntry{{}}, knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"openai"}) {
		t.Fatalf("empty stage: got %+v, want [openai] narrow-safe", got)
	}
}

func TestEffectiveProviders_RouterAndHumanNodes(t *testing.T) {
	// A router in `llm` mode spends through its embedded LLMFields — it
	// resolves like an agent. A router in a deterministic mode spends
	// nothing and must not widen.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a": agentWith("a", "openai", ""),
		"r": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "r"}, RouterMode: ir.RouterLLM, LLMFields: ir.LLMFields{Provider: "anthropic"}},
		"f": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "f"}, RouterMode: ir.RouterFanOutAll},
	}}
	got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest)
	if !got.NarrowSafe || !slicesEqual(got.Providers, []string{"anthropic", "openai"}) {
		t.Fatalf("llm router: got %+v, want [anthropic openai] narrow-safe", got)
	}

	// A human node answering with a model exposes no LLMFields: it
	// spends on a provider the walk cannot name, so it widens.
	wf.Nodes["h"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "h"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLM}}
	got = EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest)
	if got.NarrowSafe {
		t.Fatalf("a model-answering human node must widen: %+v", got)
	}
	// A plain human node does not.
	wf.Nodes["h"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "h"}}
	if got = EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest); !got.NarrowSafe {
		t.Fatalf("a human-only node must not widen: %+v", got)
	}
}

func TestEffectiveProviders_NoLLMNodeAndNilInputs(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"t": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}}}}
	if got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest); !got.NarrowSafe || len(got.Providers) != 0 {
		t.Fatalf("tool-only workflow: got %+v, want empty and narrow-safe", got)
	}
	if got := EffectiveProviders(nil, ModelOverrides{}, nil, knownForTest); got.NarrowSafe {
		t.Fatalf("nil workflow must fail open: %+v", got)
	}
	if got := EffectiveProviders(wf, ModelOverrides{}, nil, nil); got.NarrowSafe {
		t.Fatalf("empty vocabulary must fail open: %+v", got)
	}
}

func TestOverridesFrom_FoldsEveryField(t *testing.T) {
	o := OverridesFrom([]OverrideEntry{
		{Selector: "judge", Backend: "claw", Model: "openai/gpt-5", Provider: "openai"},
		{Selector: "plan", Model: "claude-opus-5"},
	})
	j := o.ForNode("verdict", ir.NodeJudge)
	if j.Backend != "claw" || j.Model != "openai/gpt-5" || j.Provider != "openai" {
		t.Fatalf("judge-kind selector: got %+v", j)
	}
	p := o.ForNode("plan", ir.NodeAgent)
	if p.Model != "claude-opus-5" || p.Backend != "" || p.Provider != "" {
		t.Fatalf("node selector: got %+v", p)
	}
	if !OverridesFrom(nil).Empty() {
		t.Fatal("no entries must fold to an empty override set")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Round 2 S1: the hint IS the route. The executor's resolveProviderChain
// never reads the model's `provider/` prefix — on claude_code a `zai` hint
// spends ONLY the z.ai key (and the `anthropic/` prefix is stripped), on
// pi the hint overrides the prefix and `model: "anthropic/glm-5.2"` +
// `provider: "zai"` is THE documented z.ai form. Round 1's walk returned
// on the prefix first and reported [anthropic] for every one of these,
// so the only donor able to serve (a z.ai key) was never considered.
func TestEffectiveProviders_HintWinsOverModelPrefix(t *testing.T) {
	t.Setenv("RESCUE_PROVIDER", "")
	nodeOf := func(backend, provider, mdl string) *ir.AgentNode {
		return &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: backend, Provider: provider, Model: mdl}}
	}
	rows := []struct {
		name      string
		node      *ir.AgentNode
		overrides ModelOverrides
		want      []string
	}{
		{"claude_code + anthropic/ prefix + zai hint → zai", nodeOf("claude_code", "zai", "anthropic/claude-opus-4-8"), ModelOverrides{}, []string{"zai"}},
		{"claude_code + anthropic/ prefix + chain zai,anthropic → the chain", nodeOf("claude_code", "zai,anthropic", "anthropic/claude-opus-4-8"), ModelOverrides{}, []string{"anthropic", "zai"}},
		{"pi documented z.ai form: anthropic/glm-5.2 + zai → zai", nodeOf("pi", "zai", "anthropic/glm-5.2"), ModelOverrides{}, []string{"zai"}},
		{"secured-renovacy shape with an anthropic/ model → zai", nodeOf("claude_code", "${RESCUE_PROVIDER:-zai}", "anthropic/claude-opus-4-8"), ModelOverrides{}, []string{"zai"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": row.node}}
			got := EffectiveProviders(wf, row.overrides, nil, knownForTest)
			if !got.NarrowSafe || !slicesEqual(got.Providers, row.want) {
				t.Fatalf("got %+v, want %v narrow-safe", got, row.want)
			}
		})
	}

	// A launch `--model agent=anthropic/…` over a `provider: zai` node:
	// the executor keeps the DSL chain (only a PROVIDER override collapses
	// it), so the wants still say zai.
	var modelOnly ModelOverrides
	modelOnly.SetModel("a", "anthropic/claude-opus-4-8")
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": nodeOf("claude_code", "zai", "")}}
	if got := EffectiveProviders(wf, modelOnly, nil, knownForTest); !got.NarrowSafe || !slicesEqual(got.Providers, []string{"zai"}) {
		t.Fatalf("model override over a zai hint: got %+v, want [zai]", got)
	}
	// A PROVIDER override does collapse it.
	var provOv ModelOverrides
	provOv.SetProvider("a", "anthropic")
	if got := EffectiveProviders(wf, provOv, nil, knownForTest); !got.NarrowSafe || !slicesEqual(got.Providers, []string{"anthropic"}) {
		t.Fatalf("provider override over a zai hint: got %+v, want [anthropic]", got)
	}
	// No hint at all: the prefix routes (claw's registry).
	wf = &ir.Workflow{Nodes: map[string]ir.Node{"a": nodeOf("claw", "auto", "openai/gpt-5")}}
	if got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest); !got.NarrowSafe || !slicesEqual(got.Providers, []string{"openai"}) {
		t.Fatalf("auto hint + prefix: got %+v, want [openai]", got)
	}
	// A fallback route follows the same rule: its hint beats its prefix.
	wf = &ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Backend: "claude_code", Provider: "anthropic"},
		Fallbacks: []ir.Fallback{{Name: "rescue", Provider: "zai", Model: "anthropic/glm-5.2"}},
	}}}
	if got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest); !got.NarrowSafe || !slicesEqual(got.Providers, []string{"anthropic", "zai"}) {
		t.Fatalf("fallback hint over prefix: got %+v, want [anthropic zai]", got)
	}
}

// Round 2 S11: `interaction: llm` answers the node's questions with a
// DIRECT generation on `interaction_model` (falling back to the node's
// model) — a route no hint applies to. Its prefix joins the wants; a
// spec with no prefix widens.
func TestEffectiveProviders_InteractionModelIsARoute(t *testing.T) {
	node := func(interaction ir.InteractionMode, interactionModel, mdl string) *ir.AgentNode {
		return &ir.AgentNode{
			BaseNode:          ir.BaseNode{ID: "a"},
			LLMFields:         ir.LLMFields{Backend: "claude_code", Provider: "zai", Model: mdl},
			InteractionFields: ir.InteractionFields{Interaction: interaction, InteractionModel: interactionModel},
		}
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": node(ir.InteractionLLM, "openai/gpt-5", "")}}
	if got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest); !got.NarrowSafe || !slicesEqual(got.Providers, []string{"openai", "zai"}) {
		t.Fatalf("interaction_model openai/…: got %+v, want [openai zai]", got)
	}
	wf = &ir.Workflow{Nodes: map[string]ir.Node{"a": node(ir.InteractionLLMOrHuman, "", "anthropic/claude-opus-4-8")}}
	if got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest); !got.NarrowSafe || !slicesEqual(got.Providers, []string{"anthropic", "zai"}) {
		t.Fatalf("interaction falls back to the node model's prefix: got %+v, want [anthropic zai]", got)
	}
	wf = &ir.Workflow{Nodes: map[string]ir.Node{"a": node(ir.InteractionLLM, "", "")}}
	if got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest); got.NarrowSafe {
		t.Fatalf("interaction on a bare model spec must widen: got %+v", got)
	}
	wf = &ir.Workflow{Nodes: map[string]ir.Node{"a": node(ir.InteractionHuman, "openai/gpt-5", "")}}
	if got := EffectiveProviders(wf, ModelOverrides{}, nil, knownForTest); !got.NarrowSafe || !slicesEqual(got.Providers, []string{"zai"}) {
		t.Fatalf("a human interaction spends nothing on interaction_model: got %+v, want [zai]", got)
	}
}
