package model

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ForfaitFirst is a POSITIVE list: a provider enters it only when every route
// naming it runs on a backend that declared it spends that provider's forfait
// first. Anything the walk cannot vouch for keeps it out — the direction in
// which the accounting counts a pinned key rather than forgets one.
func TestEffectiveProvidersForfaitFirstIsPositive(t *testing.T) {
	known := map[string]bool{"anthropic": true, "openai": true, "zai": true}
	t.Setenv("FORFAIT_FIRST_WALK_UNSET_BACKEND", "")
	agent := func(id, backend, provider, model string) ir.Node {
		return &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, LLMFields: ir.LLMFields{Backend: backend, Provider: provider, Model: model}}
	}
	for _, tc := range []struct {
		name  string
		wf    *ir.Workflow
		want  []string // ForfaitFirst
		wantP []string // Providers
	}{
		{"claude_code pinned anthropic",
			&ir.Workflow{Nodes: map[string]ir.Node{"a": agent("a", "claude_code", "anthropic", "claude-opus-5-5")}},
			[]string{"anthropic"}, []string{"anthropic"}},
		{"codex on openai",
			&ir.Workflow{Nodes: map[string]ir.Node{"a": agent("a", "codex", "", "openai/gpt-6-sol")}},
			[]string{"openai"}, []string{"openai"}},
		// claw's openai order depends on the runner's OAuth settings, which
		// this walk cannot see: it vouches for nothing, even beside codex.
		{"claw on openai beside codex",
			&ir.Workflow{Nodes: map[string]ir.Node{
				"a": agent("a", "claw", "", "openai/gpt-6"),
				"b": agent("b", "codex", "", "openai/gpt-6-sol"),
			}},
			nil, []string{"openai"}},
		// A backend read from the environment is this process's reading of
		// it; the runner expands it with its own.
		{"a backend read from the environment",
			&ir.Workflow{Nodes: map[string]ir.Node{"a": agent("a", "${FORFAIT_FIRST_WALK_BACKEND:-claude_code}", "anthropic", "claude-opus-5-5")}},
			nil, []string{"anthropic"}},
		// Unset here, the runner may still set it: the workflow default this
		// process falls through to is not a promise.
		{"a backend read from an environment variable unset here",
			&ir.Workflow{DefaultBackend: "claude_code", Nodes: map[string]ir.Node{"a": agent("a", "${FORFAIT_FIRST_WALK_UNSET_BACKEND}", "anthropic", "claude-opus-5-5")}},
			nil, []string{"anthropic"}},
		{"a claw route on anthropic too",
			&ir.Workflow{Nodes: map[string]ir.Node{
				"a": agent("a", "claude_code", "anthropic", "claude-opus-5-5"),
				"b": agent("b", "claw", "", "anthropic/claude-opus-5-5"),
			}},
			nil, []string{"anthropic"}},
		{"the workflow default backend",
			&ir.Workflow{DefaultBackend: "claude_code", Nodes: map[string]ir.Node{"a": agent("a", "", "anthropic", "claude-opus-5-5")}},
			[]string{"anthropic"}, []string{"anthropic"}},
		{"a backend resolved at dispatch",
			&ir.Workflow{Nodes: map[string]ir.Node{"a": agent("a", "{{vars.backend}}", "anthropic", "claude-opus-5-5")}},
			nil, []string{"anthropic"}},
		{"a fallback route on its own backend",
			&ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "a"},
				LLMFields: ir.LLMFields{Backend: "claude_code", Provider: "anthropic", Model: "claude-opus-5-5"},
				Fallbacks: []ir.Fallback{{Backend: "claw", Model: "anthropic/claude-opus-5-5"}},
			}}},
			nil, []string{"anthropic"}},
		{"a fallback route on the node's backend",
			&ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "a"},
				LLMFields: ir.LLMFields{Backend: "claude_code", Provider: "zai", Model: "glm-5.3"},
				Fallbacks: []ir.Fallback{{Provider: "anthropic", Model: "claude-opus-5-5"}},
			}}},
			[]string{"anthropic"}, []string{"anthropic", "zai"}},
		// A claude_code rescue does not vouch for the claw primary route on
		// the same provider — whatever the order the walk reads them in.
		{"a forfait-first fallback after a key-bound primary",
			&ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "a"},
				LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5-5"},
				Fallbacks: []ir.Fallback{{Backend: "claude_code", Provider: "anthropic", Model: "claude-opus-5-5"}},
			}}},
			nil, []string{"anthropic"}},
		// A supervisor calls its model in process for the whole run: the
		// in-process registry spends a key pinned for its route before the
		// forfait, so it vouches for nothing — and one with no model of
		// its own resolves on the runner, which the walk cannot name.
		{"a supervisor beside a claude_code route",
			&ir.Workflow{
				Nodes:       map[string]ir.Node{"a": agent("a", "claude_code", "anthropic", "claude-opus-5-5")},
				Supervisors: []*ir.Supervisor{{Name: "coach", Model: "anthropic/claude-haiku-4-5"}},
			},
			nil, []string{"anthropic"}},
		{"a direct generation reads no bundle",
			&ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
				BaseNode:          ir.BaseNode{ID: "a"},
				LLMFields:         ir.LLMFields{Backend: "claude_code", Provider: "anthropic", Model: "claude-opus-5-5"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLM, InteractionModel: "anthropic/claude-haiku-4-5"},
			}}},
			nil, []string{"anthropic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := EffectiveProviders(tc.wf, ModelOverrides{}, nil, known)
			if !slices.Equal(res.ForfaitFirst, tc.want) {
				t.Errorf("ForfaitFirst = %v, want %v", res.ForfaitFirst, tc.want)
			}
			if !slices.Equal(res.Providers, tc.wantP) {
				t.Errorf("Providers = %v, want %v", res.Providers, tc.wantP)
			}
		})
	}
	if got := (ProviderResolution{}).ForfaitFirst; len(got) != 0 {
		t.Errorf("the zero value claims %v", got)
	}
	unpinned := &ir.Workflow{
		Nodes:       map[string]ir.Node{"a": agent("a", "claude_code", "anthropic", "claude-opus-5-5")},
		Supervisors: []*ir.Supervisor{{Name: "coach"}},
	}
	if res := EffectiveProviders(unpinned, ModelOverrides{}, nil, known); res.NarrowSafe {
		t.Error("a supervisor with no model of its own left the walk narrow-safe — it spends whatever the runner resolves")
	}
}
