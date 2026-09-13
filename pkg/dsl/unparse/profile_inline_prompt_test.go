package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// An Inline prompt is written back on its property, as a quoted string
// that carries the body VERBATIM — paragraph breaks, leading indentation
// and trailing newline included, in either profile — never as a `prompt`
// declaration; and the save guard, which canonicalises every declared
// prompt, compares it as it is.
func TestUnparseReInlinesAnInlinePrompt(t *testing.T) {
	bodies := []string{
		"Review the diff",
		"First paragraph.\n\nSecond paragraph.\n",
		"    indented first line\n  less\n",
		"tab\there and a \"quote\"",
	}
	for _, profile := range []int{1, 2} {
		for _, body := range bodies {
			t.Run(body, func(t *testing.T) {
				f := &ast.File{
					Profile: profile,
					Prompts: []*ast.PromptDecl{{Name: parser.InlinePromptName(body), Body: body, Inline: true}},
					Agents:  []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{System: parser.InlinePromptName(body)}}},
					Workflows: []*ast.WorkflowDecl{{
						Name: "w", Entry: "a", Edges: []*ast.Edge{{From: "a", To: "done"}},
					}},
				}
				text := unparse.Unparse(f)
				if strings.Contains(text, "prompt _inline_") {
					t.Fatalf("an inline prompt was written as a declaration:\n%s", text)
				}
				if !strings.Contains(text, "  system: ") {
					t.Fatalf("no system property written:\n%s", text)
				}
				pr := parser.Parse("x.bot", text)
				if len(pr.Diagnostics) != 0 {
					t.Fatalf("does not parse back: %v\n%s", pr.Diagnostics, text)
				}
				if len(pr.File.Prompts) != 1 || !pr.File.Prompts[0].Inline || pr.File.Prompts[0].Body != body {
					t.Fatalf("read back as %+v", pr.File.Prompts)
				}
				if err := unparse.Verify(f, text); err != nil {
					t.Fatalf("Verify: %v\n%s", err, text)
				}
			})
		}
	}
}

// The studio's document is the JSON transport: the Inline mark rides it,
// so a document opened from a `system: "…"` file saves back to the same
// shape.
func TestInlineMarkSurvivesTheJSONTransport(t *testing.T) {
	src := "agent a:\n  system: \"Review the diff\"\n\nworkflow w:\n  entry: a\n  a -> done\n"
	pr := parser.Parse("x.bot", src)
	data, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"inline": true`) {
		t.Fatalf("the mark is not on the transport: %s", data)
	}
	back, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatal(err)
	}
	text := unparse.Unparse(back)
	if !strings.Contains(text, "  system: \"Review the diff\"\n") || strings.Contains(text, "prompt _inline_") {
		t.Fatalf("the transported document does not write back inline:\n%s", text)
	}
	if err := unparse.Verify(back, text); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}
