package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// promptDoc is a document whose prompt p is referenced by a node, so the
// guard compares compiled prompts. The prompt alone (no workflow) is the
// other path, where the guard compares the AST mirror.
func promptDoc(body string) *ast.File {
	return &ast.File{
		Prompts:   []*ast.PromptDecl{{Name: "p", Body: body}},
		Agents:    []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{Model: "m", System: "p"}}},
		Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "a", Edges: []*ast.Edge{{From: "a", To: "done"}}}},
	}
}

// A canvas prompt is a raw textarea value — two paragraphs, an Enter after
// the last line, a CRLF or a lone CR from a paste, tab-indented code as the
// first line, an indented first line — and the v1 syntax carries none of
// it as written: the lexer skips every blank line under a prompt header,
// folds a CR away with the newline, takes the first line's indentation as
// the body's. The guard compares what the syntax CAN carry, the lexer's
// canonical form, so a normal prompt saves; refusing it closed the studio's
// main authoring path.
func TestVerifyAcceptsPromptBodiesTheSyntaxCannotCarry(t *testing.T) {
	shapes := []string{
		"Hello\n", "Hello\n\nWorld", "\nHello", "Hello\r\nWorld", "Hello\n   \nWorld", "Hello\n\n", "\n\n",
		// A trailing CR is folded away with the newline; the form has to be
		// stable under a second round-trip, so every trailing CR goes.
		"Hello\r", "Hello\r\r\nWorld", "\r", "lo\rne",
		// Tab-indented code pasted as the first line: the lexer's tab guard
		// must not fire on a line that opens a prompt body.
		"\tif err != nil {\n\t\treturn err\n\t}", "  \ta\n  b", "\n\n\tfirst\nsecond",
		// The first line's indentation is the body's; deeper lines keep the
		// difference.
		"    code\n    more", "  a\n    b",
	}
	for _, body := range shapes {
		for name, f := range map[string]*ast.File{
			"referenced": promptDoc(body),
			"alone":      {Prompts: []*ast.PromptDecl{{Name: "p", Body: body}}},
		} {
			text := Unparse(f)
			if err := Verify(f, text); err != nil {
				t.Errorf("%s %q: refused: %v\n%s", name, body, err, text)
				continue
			}
			pr := parser.Parse("", text)
			if len(pr.File.Prompts) != 1 {
				t.Fatalf("%s %q: the prompt did not survive: %+v", name, body, pr.File.Prompts)
			}
			if got, want := pr.File.Prompts[0].Body, parser.CanonicalPromptBody(body); got != want {
				t.Errorf("%s %q: reads back as %q, want the canonical %q", name, body, got, want)
			}
		}
	}
}

// A body whose first line is indented deeper than a later one has NO
// written form: the lexer takes the first line's indentation as the body's
// and ends the body at a shallower line. Writing its nearest form would
// de-indent the prompt the model receives — a change of program — so the
// guard refuses it by name, with the line, instead of passing it.
func TestVerifyRefusesAPromptBodyTheSyntaxCannotCarry(t *testing.T) {
	for _, body := range []string{"    a\n  b", "    Given:\n  - a\n  - b", "        first\n    less\n        deep", "  \ta\nb"} {
		for name, f := range map[string]*ast.File{
			"referenced": promptDoc(body),
			"alone":      {Prompts: []*ast.PromptDecl{{Name: "p", Body: body}}},
		} {
			err := Verify(f, Unparse(f))
			if err == nil {
				t.Errorf("%s %q: a body the syntax cannot carry was accepted, de-indented", name, body)
				continue
			}
			if !strings.Contains(err.Error(), `prompt "p"`) || !strings.Contains(err.Error(), "indented") {
				t.Errorf("%s %q: the refusal does not name the prompt and the cause: %v", name, body, err)
			}
		}
	}
}

// The text written is the canonical form itself: no space-only line under
// the header, no indented blank at the end. What is on disk says what it
// means, and a second unparse of the re-parse is byte-identical.
func TestUnparseWritesPromptBodiesInCanonicalForm(t *testing.T) {
	f := &ast.File{Prompts: []*ast.PromptDecl{{Name: "p", Body: "Hello\n\n   \nWorld\n\n"}}}
	text := Unparse(f)
	if want := "prompt p:\n  Hello\n  World\n"; text != want {
		t.Fatalf("got %q, want %q", text, want)
	}
	if again := Unparse(parser.Parse("", text).File); again != text {
		t.Fatalf("the round-trip is not stable: %q then %q", text, again)
	}
}

// The workflow is the one declaration the canvas can leave empty — right
// after its last node is deleted, or freshly scaffolded — whose bare
// header the parser could not read back.
func TestVerifyAcceptsAnEmptyWorkflow(t *testing.T) {
	for name, f := range map[string]*ast.File{
		"alone":              {Workflows: []*ast.WorkflowDecl{{Name: "w"}}},
		"after declarations": {Prompts: []*ast.PromptDecl{{Name: "p", Body: "x"}}, Workflows: []*ast.WorkflowDecl{{Name: "w"}}},
	} {
		text := Unparse(f)
		if err := Verify(f, text); err != nil {
			t.Errorf("%s: %v\n%s", name, err, text)
		}
	}
}
