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
