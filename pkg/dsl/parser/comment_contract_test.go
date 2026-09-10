package parser_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// The lexer is one of four readers of "what a comment line looks like" (the
// strict-escape pre-scan, the bundle frontmatter reader and the catalogue's
// description reader are the others). Accepting `#` at the lexer alone left
// the other three reading `##` only — silently: a `# note` above
// `## strict-escape: on` disarmed the directive, and a `# ---` frontmatter
// lost the bot's catalogue identity. workflowfile.CommentText is the shared
// definition; this test holds the lexer to it.
func TestLexerCommentTextMatchesSharedDefinition(t *testing.T) {
	for _, line := range []string{"# note", "## note", "#note", "##   indented", "   # leading spaces", "#", "##"} {
		want, ok := workflowfile.CommentText(line)
		if !ok {
			t.Fatalf("%q must be a comment line for the shared definition", line)
		}
		lx := parser.NewLexer("c.bot", line+"\n")
		var got string
		var found bool
		for _, tok := range lx.All() {
			if tok.Type == parser.TokenComment {
				got, found = tok.Value, true
			}
		}
		if !found {
			t.Errorf("%q: the lexer emitted no comment token", line)
			continue
		}
		if got != strings.TrimSpace(want) {
			t.Errorf("%q: lexer text %q, shared definition %q", line, got, want)
		}
	}
	if _, ok := workflowfile.CommentText("key: value"); ok {
		t.Error("a property line is not a comment")
	}
	// The lexer refuses a tab as indentation, so a tab-indented `#` line is
	// not a comment line for the shared definition either — otherwise a
	// file the parser rejects would still be read for its frontmatter.
	if _, ok := workflowfile.CommentText("\t# tabbed"); ok {
		t.Error("a tab-indented hash line must not be a comment line")
	}
}

// `## strict-escape: on` is read by a pre-scan of the leading comment lines.
// That scan must accept the same comment forms as the lexer, or a valid file's
// string semantics depend on the hash count of an unrelated comment.
func TestStrictEscapeDirectiveSurvivesSingleHashComments(t *testing.T) {
	body := "\nschema out:\n  ok: bool\n  note: string\n\nagent a:\n  model: \"m\\tx\"\n  output: out\n\nworkflow w:\n  entry: a\n  a -> done\n"
	cases := []struct {
		name   string
		header string
		strict bool
	}{
		{"double-hash directive", "## strict-escape: on\n", true},
		{"single-hash directive", "# strict-escape: on\n", true},
		{"single-hash comment above the directive", "# plain comment\n## strict-escape: on\n", true},
		{"no directive", "# plain comment\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := parser.Parse("se.bot", tc.header+body)
			for _, d := range pr.Diagnostics {
				t.Fatalf("unexpected diagnostic: %s", d.Error())
			}
			model := pr.File.Agents[0].Model
			if tc.strict && model != "m\tx" {
				t.Errorf("strict mode expected: model = %q, want a real tab", model)
			}
			if !tc.strict && model != `m\tx` {
				t.Errorf("legacy mode expected: model = %q, want the literal backslash-t", model)
			}
		})
	}
}

// A comment on the prompt header line is not part of the header: the body
// that follows still opens as prompt text, first line included.
func TestPromptHeaderTrailingCommentKeepsTheBody(t *testing.T) {
	for _, header := range []string{"prompt p: # why", "prompt p: ## why", "prompt p:"} {
		src := "schema out:\n  ok: bool\n\n" + header + "\n  # Heading of the body\n  Body text.\n\nagent a:\n  model: \"m\"\n  output: out\n  system: p\n\nworkflow w:\n  entry: a\n  a -> done\n"
		pr := parser.Parse("ph.bot", src)
		for _, d := range pr.Diagnostics {
			t.Errorf("%q: unexpected diagnostic: %s", header, d.Error())
		}
		if pr.File == nil || len(pr.File.Prompts) != 1 {
			t.Fatalf("%q: expected one prompt", header)
		}
		if b := pr.File.Prompts[0].Body; !strings.Contains(b, "# Heading of the body") || !strings.Contains(b, "Body text.") {
			t.Errorf("%q: prompt body lost a line: %q", header, b)
		}
	}
}
