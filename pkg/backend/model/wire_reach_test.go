package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The usage cap meters the Anthropic wire only (its readings come from
// the claude_code delegate). A run parked "for the anthropic weekly reset"
// while every one of its routes is claw/openai is the #668 incident;
// every uncertainty must keep the guard armed instead.
func TestAnthropicWireReachable(t *testing.T) {
	t.Setenv("RESCUE_PROVIDER", "")
	wfOf := func(nodes ...ir.Node) *ir.Workflow {
		wf := &ir.Workflow{Nodes: map[string]ir.Node{}}
		for _, n := range nodes {
			wf.Nodes[n.NodeID()] = n
		}
		return wf
	}
	cases := []struct {
		name string
		wf   *ir.Workflow
		want bool
	}{
		{"nil workflow is unknown", nil, true},
		{"tool-only spends nothing", wfOf(&ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}}), false},
		{"unpinned node may resolve to claude_code", wfOf(agentWith("a", "", "")), true},
		{"claude_code backend", wfOf(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claude_code", Provider: "openai"}}), true},
		{"workflow default_backend claude_code", &ir.Workflow{DefaultBackend: "claude_code", Nodes: map[string]ir.Node{"a": agentWith("a", "openai", "")}}, true},
		{"claw + openai model + openai provider (probe 1/2)", wfOf(
			&ir.AgentNode{BaseNode: ir.BaseNode{ID: "oracle"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "openai", Model: "openai/gpt-5.6-sol"}},
			&ir.JudgeNode{BaseNode: ir.BaseNode{ID: "mutants"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "openai", Model: "openai/gpt-5.6-sol"}},
		), false},
		{"claw + openai model prefix only", wfOf(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai/gpt-5"}}), false},
		{"claw + anthropic provider", wfOf(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "anthropic"}}), true},
		{"claw + zai facade", wfOf(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "${RESCUE_PROVIDER:-zai}"}}), true},
		{"claw with no provider substitutes what the process holds", wfOf(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw"}}), true},
		{"claw chain openai,anthropic reaches the wire", wfOf(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "openai,anthropic"}}), true},
		{"codex is bound to openai", wfOf(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "codex"}}), false},
		{"kimi and grok are bound to their vendor", wfOf(
			&ir.AgentNode{BaseNode: ir.BaseNode{ID: "k"}, LLMFields: ir.LLMFields{Backend: "kimi", Model: "kimi-code/kimi-for-coding"}},
			&ir.AgentNode{BaseNode: ir.BaseNode{ID: "g"}, LLMFields: ir.LLMFields{Backend: "grok"}},
		), false},
		{"pi + anthropic provider", wfOf(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "pi", Provider: "anthropic"}}), true},
		{"pi + openai provider", wfOf(&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "pi", Provider: "openai"}}), false},
		{"one off-wire node next to one unpinned node", wfOf(
			&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "openai"}},
			agentWith("b", "", ""),
		), true},
		{"llm router on the wire", wfOf(
			&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "openai"}},
			&ir.RouterNode{BaseNode: ir.BaseNode{ID: "r"}, RouterMode: ir.RouterLLM, LLMFields: ir.LLMFields{Backend: "claude_code"}},
		), true},
		{"deterministic router spends nothing", wfOf(
			&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "openai"}},
			&ir.RouterNode{BaseNode: ir.BaseNode{ID: "r"}, RouterMode: ir.RouterFanOutAll},
		), false},
		{"human answering with a model is unknowable", wfOf(
			&ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "openai"}},
			&ir.HumanNode{BaseNode: ir.BaseNode{ID: "h"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLM}},
		), true},
		{"node fallback route onto claude_code", wfOf(&ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "a"},
			LLMFields: ir.LLMFields{Backend: "claw", Provider: "openai", Model: "openai/gpt-5"},
			Fallbacks: []ir.Fallback{{Name: "rescue", Backend: "claude_code"}},
		}), true},
		{"node fallback route inheriting an off-wire model", wfOf(&ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "a"},
			LLMFields: ir.LLMFields{Backend: "claw", Provider: "openai", Model: "openai/gpt-5"},
			Fallbacks: []ir.Fallback{{Name: "cheaper", Model: "openai/gpt-5-mini"}},
		}), false},
		{"skip route executes nothing", wfOf(&ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "a"},
			LLMFields: ir.LLMFields{Backend: "claw", Provider: "openai", Model: "openai/gpt-5"},
			Fallbacks: []ir.Fallback{{Name: "give-up", Action: ir.FallbackActionSkip}},
		}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AnthropicWireReachable(tc.wf, ModelOverrides{}, nil); got != tc.want {
				t.Fatalf("AnthropicWireReachable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAnthropicWireReachable_OverridesAndRunFallback(t *testing.T) {
	// #668 exactly: a DSL-unpinned judge would resolve to claude_code, but
	// the launch pinned both the agent and the judge kind to claw/openai.
	// The overrides are what the executor will honour, so the predicate
	// must read them.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"oracle_campaign":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "oracle_campaign"}},
		"mutants_adversary": &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "mutants_adversary"}},
	}}
	if !AnthropicWireReachable(wf, ModelOverrides{}, nil) {
		t.Fatal("unpinned run must count as on the wire")
	}
	var both ModelOverrides
	for _, sel := range []string{"oracle_campaign", "mutants_adversary"} {
		both.SetBackend(sel, "claw")
		both.SetModel(sel, "openai/gpt-5.6-sol")
		both.SetProvider(sel, "openai")
	}
	if AnthropicWireReachable(wf, both, nil) {
		t.Fatal("both nodes pinned to claw/openai by override: the run cannot touch the wire")
	}
	// Pinning the agent only leaves the judge on claude_code.
	var agentOnly ModelOverrides
	agentOnly.SetBackend("oracle_campaign", "claw")
	agentOnly.SetProvider("oracle_campaign", "openai")
	if !AnthropicWireReachable(wf, agentOnly, nil) {
		t.Fatal("the judge still resolves to claude_code")
	}
	// A run-level --fallback onto claude_code re-opens the wire.
	if !AnthropicWireReachable(wf, both, []FallbackEntry{{Backend: "claude_code"}}) {
		t.Fatal("a run-level fallback onto claude_code is a route the run may take")
	}
	if AnthropicWireReachable(wf, both, []FallbackEntry{{Backend: "claw", Provider: "openai", Model: "openai/gpt-5"}}) {
		t.Fatal("a run-level fallback that stays off the wire must not re-arm the guard")
	}
}
