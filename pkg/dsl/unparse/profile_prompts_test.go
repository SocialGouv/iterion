package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// A profile-2 document with a paragraph break in a prompt is written with
// the blank line — never indented — and reads back with the break; the
// same document in profile 1 is written without it, as before, and each
// satisfies its own save guard.
func TestUnparseKeepsPromptParagraphsInProfileTwo(t *testing.T) {
	body := "First paragraph\ncontinues.\n\nSecond paragraph.\n"
	v2 := &ast.File{
		Profile: 2,
		Prompts: []*ast.PromptDecl{{Name: "p", Body: body}},
		Agents:  []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{System: "p"}}},
		Workflows: []*ast.WorkflowDecl{{
			Name: "w", Entry: "a", Edges: []*ast.Edge{{From: "a", To: "done"}},
		}},
	}
	text := unparse.Unparse(v2)
	if !strings.Contains(text, "  continues.\n\n  Second paragraph.\n") {
		t.Fatalf("the paragraph break is not written blank:\n%s", text)
	}
	pr := parser.Parse("p.bot", text)
	if len(pr.Diagnostics) != 0 {
		t.Fatalf("does not parse back: %v\n%s", pr.Diagnostics, text)
	}
	if got := pr.File.Prompts[0].Body; got != "First paragraph\ncontinues.\n\nSecond paragraph." {
		t.Fatalf("body read back as %q", got)
	}
	if err := unparse.Verify(v2, text); err != nil {
		t.Fatalf("Verify (profile 2): %v", err)
	}

	v1 := *v2
	v1.Profile = 0
	text1 := unparse.Unparse(&v1)
	if strings.Contains(text1, "continues.\n\n") {
		t.Fatalf("profile 1 wrote a blank line inside a prompt body:\n%s", text1)
	}
	if err := unparse.Verify(&v1, text1); err != nil {
		t.Fatalf("Verify (profile 1): %v", err)
	}
}
