package parser_test

// An external test package: it compiles the parsed file through pkg/dsl/ir,
// which reaches pkg/bundle, which reads the parser — a cycle for an
// in-package test, not for this one.

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// `system: "Review the diff"` — the shape the language's own front page
// taught and every first draft writes — is the prompt's text itself: it
// becomes an Inline prompt of the file, named after its body, and the
// property refers to it. On every kind that carries a prompt reference.
func TestAStringOnAPromptPropertyIsAnInlinePrompt(t *testing.T) {
	src := "agent a:\n  system: \"Review the diff\"\n  user: |\n    First paragraph.\n\n    Second paragraph.\n\nhuman h:\n  instructions: `Decide.`\n\nrouter r:\n  mode: llm\n  system: \"Route it\"\n\nsupervisor s:\n  watches: [a]\n  system: \"Watch.\"\n\nworkflow w:\n  entry: a\n  a -> r\n  r -> h\n  r -> done\n  h -> done\n"
	res := parser.Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	byName := map[string]string{}
	for _, p := range res.File.Prompts {
		if !p.Inline {
			t.Fatalf("prompt %q is not marked Inline", p.Name)
		}
		byName[p.Name] = p.Body
	}
	if len(byName) != 5 {
		t.Fatalf("want 5 inline prompts, got %d: %v", len(byName), byName)
	}
	a := res.File.Agents[0]
	// A block scalar keeps its blank lines to the less-indented line that
	// ends it — the separator before `human h:` included.
	if byName[a.System] != "Review the diff" || byName[a.User] != "First paragraph.\n\nSecond paragraph.\n\n" {
		t.Fatalf("agent refers to %q=%q and %q=%q", a.System, byName[a.System], a.User, byName[a.User])
	}
	if !strings.HasPrefix(a.System, "_inline_") || a.System != parser.InlinePromptName("Review the diff") {
		t.Fatalf("inline name %q is not derived from the body", a.System)
	}
	if byName[res.File.Humans[0].Instructions] != "Decide." || byName[res.File.Routers[0].System] != "Route it" || byName[res.File.Supervisors[0].System] != "Watch." {
		t.Fatalf("human/router/supervisor references: %+v", byName)
	}
	// The compiler resolves them like any prompt.
	c := ir.Compile(res.File)
	if c.Workflow == nil {
		t.Fatalf("does not compile: %v", c.Diagnostics)
	}
	for _, d := range c.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("compile error: %v", d)
		}
	}
	if got := c.Workflow.Prompts[a.System]; got == nil || got.Body != "Review the diff" {
		t.Fatalf("compiled prompt: %+v", got)
	}
}

// The name is the body's: two references to the same text share one
// prompt (in two groups, on two nodes, whatever their names), a node's
// rename leaves it untouched, and a bare name still refers to a declared
// prompt.
func TestInlinePromptsAreNamedByTheirBody(t *testing.T) {
	src := "group g:\n  agent worker:\n    system: \"Same text\"\n  worker -> done\n\ngroup h:\n  agent worker:\n    system: \"Same text\"\n  worker -> done\n\nagent b:\n  system: \"Same text\"\n\nprompt p:\n  Declared.\n\nagent c:\n  system: p\n\nworkflow w:\n  entry: b\n  b -> c\n  c -> done\n"
	res := parser.Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	var inline, declared int
	for _, p := range res.File.Prompts {
		if p.Inline {
			inline++
		} else {
			declared++
		}
	}
	if inline != 1 || declared != 1 {
		t.Fatalf("want 1 inline (shared) + 1 declared prompt, got %d + %d", inline, declared)
	}
	want := parser.InlinePromptName("Same text")
	if res.File.Groups[0].Agents[0].System != want || res.File.Groups[1].Agents[0].System != want || res.File.Agents[0].System != want {
		t.Fatalf("the three references do not share the prompt")
	}
	if res.File.Agents[1].System != "p" {
		t.Fatalf("a bare name no longer refers to the declared prompt: %q", res.File.Agents[1].System)
	}
	// Renaming the node does not change the prompt's name.
	renamed := parser.Parse("y.bot", strings.ReplaceAll(src, "agent b:", "agent renamed:"))
	if renamed.File.Agents[0].System != want {
		t.Fatalf("a rename changed the inline prompt's name to %q", renamed.File.Agents[0].System)
	}
	// And two different texts are two prompts, under two names — the name
	// comes from the body, not from a counter or a constant.
	two := parser.Parse("z.bot", "agent a:\n  system: \"one\"\n\nagent b:\n  system: \"two\"\n")
	if len(two.Diagnostics) != 0 || len(two.File.Prompts) != 2 {
		t.Fatalf("two texts: %v, %d prompts", two.Diagnostics, len(two.File.Prompts))
	}
	if two.File.Agents[0].System == two.File.Agents[1].System {
		t.Fatalf("two different texts share the name %q", two.File.Agents[0].System)
	}
	if two.File.Prompts[0].Body != "one" || two.File.Prompts[1].Body != "two" {
		t.Fatalf("bodies: %q %q", two.File.Prompts[0].Body, two.File.Prompts[1].Body)
	}
}

// An author's own prompt declared under an inline name is the same
// collision as any duplicate: refused by the compiler, never merged.
func TestAnInlineNameCollidingWithADeclarationIsRefused(t *testing.T) {
	name := parser.InlinePromptName("x")
	src := "prompt " + name + ":\n  Mine.\n\nagent a:\n  system: \"x\"\n\nworkflow w:\n  entry: a\n  a -> done\n"
	res := parser.Parse("x.bot", src)
	c := ir.Compile(res.File)
	var dup bool
	for _, d := range c.Diagnostics {
		if strings.Contains(d.Message, "duplicate prompt name") {
			dup = true
		}
	}
	if !dup {
		t.Fatalf("the collision was not refused: %v", c.Diagnostics)
	}
}
