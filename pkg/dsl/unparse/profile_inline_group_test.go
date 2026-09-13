package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// A group member's inline prompt is written back inline too: the group's
// body has its own writer, which must know the file's inline prompts.
func TestUnparseReInlinesAGroupMembersPrompt(t *testing.T) {
	body := "Same text"
	name := parser.InlinePromptName(body)
	f := &ast.File{
		Prompts: []*ast.PromptDecl{{Name: name, Body: body, Inline: true}},
		Groups: []*ast.GroupDecl{{
			Name:   "g",
			Agents: []*ast.AgentDecl{{Name: "worker", LLMDecl: ast.LLMDecl{System: name}}},
			Edges:  []*ast.Edge{{From: "worker", To: "done"}},
		}},
	}
	text := unparse.Unparse(f)
	if !strings.Contains(text, "    system: \"Same text\"\n") || strings.Contains(text, "prompt _inline_") || strings.Contains(text, "system: _inline_") {
		t.Fatalf("the member's prompt is not written inline:\n%s", text)
	}
	pr := parser.Parse("g.bot", text)
	if len(pr.Diagnostics) != 0 || len(pr.File.Prompts) != 1 || pr.File.Groups[0].Agents[0].System != name {
		t.Fatalf("read back: %v %+v", pr.Diagnostics, pr.File.Prompts)
	}
	if err := unparse.Verify(f, text); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}
