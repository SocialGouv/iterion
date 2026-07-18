package ast_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// Marshal → Unmarshal must round-trip the optional node `description:`
// on every node decl kind.
func TestJSONRoundtripNodeDescriptions(t *testing.T) {
	f := &ast.File{
		Agents:   []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{Description: "Agent label", Model: "m"}}},
		Judges:   []*ast.JudgeDecl{{Name: "j", LLMDecl: ast.LLMDecl{Description: "Judge label", Model: "m"}}},
		Routers:  []*ast.RouterDecl{{Name: "r", Description: "Router label", Mode: ast.RouterFanOutAll}},
		Humans:   []*ast.HumanDecl{{Name: "h", Description: "Human label", Interaction: ast.InteractionHuman}},
		Tools:    []*ast.ToolNodeDecl{{Name: "tl", Description: "Tool label", Command: "true"}},
		Computes: []*ast.ComputeDecl{{Name: "c", Description: "Compute label", Expr: []*ast.ComputeExpr{{Key: "ok", Expr: "true"}}}},
		Subbots:  []*ast.SubbotDecl{{Name: "sb", Description: "Subbot label", Source: "child.bot"}},
		Emits:    []*ast.EmitDecl{{Name: "e", Description: "Emit label", Event: "ready"}},
		Waits:    []*ast.WaitDecl{{Name: "w", Description: "Wait label", Event: "ready", Timeout: "30s"}},
	}

	data, err := ast.MarshalFile(f)
	if err != nil {
		t.Fatalf("MarshalFile: %v", err)
	}
	got, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("UnmarshalFile: %v", err)
	}

	checks := []struct {
		kind, got, want string
	}{
		{"agent", got.Agents[0].Description, "Agent label"},
		{"judge", got.Judges[0].Description, "Judge label"},
		{"router", got.Routers[0].Description, "Router label"},
		{"human", got.Humans[0].Description, "Human label"},
		{"tool", got.Tools[0].Description, "Tool label"},
		{"compute", got.Computes[0].Description, "Compute label"},
		{"subbot", got.Subbots[0].Description, "Subbot label"},
		{"emit", got.Emits[0].Description, "Emit label"},
		{"wait", got.Waits[0].Description, "Wait label"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s Description = %q, want %q\nJSON:\n%s", c.kind, c.got, c.want, data)
		}
	}
}
