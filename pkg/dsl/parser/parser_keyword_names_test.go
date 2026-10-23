package parser_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestNodeNamedWithKeywordToken guards the parser fix that extended
// isKeywordToken to recognise the dozen-plus tokens that were defined
// as keywords but missing from the name-position whitelist. Each of
// these used to produce "expected identifier, got 'X'" for a node
// whose name collided with a DSL keyword.
func TestNodeNamedWithKeywordToken(t *testing.T) {
	// One agent declaration per previously-rejected name. The body
	// can be minimal; we only care that parsing reaches the name and
	// accepts it without a diagnostic.
	names := []string{
		"fork",
		"script",
		"language",
		"interaction",
		"interaction_prompt",
		"interaction_model",
		"default_backend",
		"multi",
		"llm",
		"round_robin",
		"auth",
		"readonly",
		"presets",
		"join",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			src := "agent " + name + ":\n  model: \"gpt-4\"\n"
			res := parser.ParseLenient("kw.bot", src)
			for _, d := range res.Diagnostics {
				t.Errorf("unexpected diagnostic on `agent %s:` -> %v", name, d)
			}
			if len(res.File.Agents) != 1 {
				t.Fatalf("expected 1 agent, got %d", len(res.File.Agents))
			}
			if got := res.File.Agents[0].Name; got != name {
				t.Errorf("agent name = %q, want %q", got, name)
			}
		})
	}
}

// TestSkipToNextTopLevel_PreservesPresetsAfterError guards that an
// error inside `vars:` no longer silently swallows a following
// `presets:` block. The skip set was missing TokenPresets +
// TokenAttachments before.
func TestSkipToNextTopLevel_PreservesPresetsAfterError(t *testing.T) {
	src := `vars:
  ## intentional syntax error to trigger skipToNextTopLevel
  ~~~

presets:
  light:
    foo: "bar"

workflow trivial:
  entry: done
`
	res := parser.ParseLenient("skip.bot", src)
	if res.File.Presets == nil {
		t.Fatalf("presets block lost after vars error; expected at least one preset to survive")
	}
}

// TestPromptNamedWithKeywordToken guards the lexer fix that lets a prompt
// whose name collides with a DSL keyword (e.g. `prompt llm:`, `prompt join:`)
// still enter prompt-body mode. Before the fix, isPromptIndent only matched
// TokenIdent, so the indented body was lexed as ordinary tokens and produced
// a cascade of "unexpected token in prompt body" diagnostics.
func TestPromptNamedWithKeywordToken(t *testing.T) {
	names := []string{"llm", "join", "auth", "readonly", "fork"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			src := "prompt " + name + ":\n  Hello from the body.\n"
			res := parser.ParseLenient("kwprompt.bot", src)
			for _, d := range res.Diagnostics {
				t.Errorf("unexpected diagnostic on `prompt %s:` -> %v", name, d)
			}
			if len(res.File.Prompts) != 1 {
				t.Fatalf("expected 1 prompt, got %d", len(res.File.Prompts))
			}
			if got := res.File.Prompts[0].Name; got != name {
				t.Errorf("prompt name = %q, want %q", got, name)
			}
			if res.File.Prompts[0].Body == "" {
				t.Errorf("prompt body empty — lexer did not enter prompt mode")
			}
		})
	}
}
