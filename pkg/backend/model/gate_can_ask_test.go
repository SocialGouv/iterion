package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestGateCanAsk_readsThePolicyTheGateArms: whether a node's gate can pause
// the run to ask is read from the policy this executor builds for the node's
// task — the run's override first — not from the IR alone: an ask imposed at
// launch counts on a node that declares nothing, ask rules count under deny,
// an override that turns the gate off wins, and a tool node is never asked.
func TestGateCanAsk_readsThePolicyTheGateArms(t *testing.T) {
	t.Setenv("ITERION_PERMISSION", "")
	agent := func(perm string, ask []string) *ir.AgentNode {
		return &ir.AgentNode{
			BaseNode:      ir.BaseNode{ID: "act"},
			LLMFields:     ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"},
			AutoMemory:    "off",
			Permission:    perm,
			PermissionAsk: ask,
		}
	}
	for _, tc := range []struct {
		name     string
		override string
		node     ir.Node
		want     bool
	}{
		{"ask imposed at launch, nothing declared", "ask", agent("", nil), true},
		{"nothing anywhere", "", agent("", nil), false},
		{"ask declared on the node", "", agent("ask", nil), true},
		{"deny with an ask rule", "", agent("deny", []string{"Bash(git push:*)"}), true},
		{"deny without an ask rule", "", agent("deny", nil), false},
		{"the run's off over the node's ask", "off", agent("ask", nil), false},
		{"a tool node under a launch ask", "ask", &ir.ToolNode{BaseNode: ir.BaseNode{ID: "lint"}, Permission: "ask"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := nodeRulesExecutor(t, &ir.Workflow{}, WithPermissionOverride(tc.override))
			if got := e.GateCanAsk(tc.node); got != tc.want {
				t.Fatalf("GateCanAsk = %v, want %v", got, tc.want)
			}
		})
	}
}
