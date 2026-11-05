package ast_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// A DECLARED empty `capabilities: []` (#1801 — the node holds NO host
// capability, overriding the workflow's list) must survive the JSON
// transport the studio canvas round-trips through: omitempty on a plain
// slice erases the declaration, and a canvas open → save would turn the
// explicit no-grant back into an inheritance. The pointer form is the one
// `tools:` already travels in.
func TestDeclaredEmptyCapabilitiesSurviveTheJSONTransport(t *testing.T) {
	original := &ast.File{
		Agents: []*ast.AgentDecl{
			{Name: "holds_none", LLMDecl: ast.LLMDecl{Model: "m", Capabilities: []string{}}},
			{Name: "inherits", LLMDecl: ast.LLMDecl{Model: "m"}},
		},
		Judges: []*ast.JudgeDecl{
			{Name: "j", LLMDecl: ast.LLMDecl{Model: "m", Capabilities: []string{}}},
		},
		Workflows: []*ast.WorkflowDecl{
			{Name: "w", Entry: "holds_none", Capabilities: []string{}},
		},
	}
	data, err := ast.MarshalFile(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if n := strings.Count(string(data), `"capabilities": []`); n != 3 {
		t.Fatalf("the three declared empty lists must be emitted (the absent one must not), found %d in %s", n, data)
	}
	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if got := restored.Agents[0].Capabilities; got == nil || len(got) != 0 {
		t.Errorf("declared empty agent Capabilities = %#v", got)
	}
	if got := restored.Agents[1].Capabilities; got != nil {
		t.Errorf("absent agent Capabilities = %#v, want nil", got)
	}
	if got := restored.Judges[0].Capabilities; got == nil || len(got) != 0 {
		t.Errorf("declared empty judge Capabilities = %#v", got)
	}
	if got := restored.Workflows[0].Capabilities; got == nil || len(got) != 0 {
		t.Errorf("declared empty workflow Capabilities = %#v", got)
	}
}

// A tool node declares `capabilities:` too (#78=A, ADR-151), with the same
// nilness rule: the canvas round-trip must keep its declared empty list
// declared, or an open → save widens the node to the workflow's grant.
func TestDeclaredEmptyToolCapabilitiesSurviveTheJSONTransport(t *testing.T) {
	original := &ast.File{
		Tools: []*ast.ToolNodeDecl{
			{Name: "holds_none", Action: "board.issue.list", Capabilities: []string{}},
			{Name: "inherits", Action: "board.issue.list"},
			{Name: "reads", Action: "board.issue.list", Capabilities: []string{"board.read"}},
		},
		Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "holds_none"}},
	}
	data, err := ast.MarshalFile(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if n := strings.Count(string(data), `"capabilities": []`); n != 1 {
		t.Fatalf("the one declared empty tool list must be emitted (the absent one must not), found %d in %s", n, data)
	}
	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if got := restored.Tools[0].Capabilities; got == nil || len(got) != 0 {
		t.Errorf("declared empty tool Capabilities = %#v", got)
	}
	if got := restored.Tools[1].Capabilities; got != nil {
		t.Errorf("absent tool Capabilities = %#v, want nil", got)
	}
	if got := strings.Join(restored.Tools[2].Capabilities, ","); got != "board.read" {
		t.Errorf("declared tool Capabilities = %q", got)
	}
}
