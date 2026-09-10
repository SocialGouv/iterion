package parser

import (
	"strings"
	"testing"
)

// The bodies a studio textarea can hold and the v1 syntax cannot carry as
// written: blank and space-only lines (the lexer skips them), a trailing
// newline, CRLF and lone CRs (folded away with the newline), tab-indented
// code as the first line, an indented first line. CanonicalPromptBody is
// the one definition of where the lexer settles; this pins it to the lexer
// itself, so a future lexer that keeps paragraph breaks turns the
// definition red instead of silently drifting from it.
var promptBodyShapes = []string{
	"Hello",
	"Hello\n",
	"Hello\n\n",
	"\nHello",
	"\n\nHello",
	"Hello\n\nWorld",
	"Hello\n\n\nWorld",
	"Hello\n   \nWorld",
	"Hello\r\nWorld",
	"Hello\r\n\r\nWorld\r\n",
	"Hello\r",
	"Hello\r\r\nWorld",
	"\r",
	"lo\rne",
	"# Heading\n\nbody",
	"## not a comment\n\n## still text",
	"Run:\n    go test ./...\n\nthen report",
	"line with trailing spaces   \n\nnext",
	"tab\tinside\n\t\nafter a tab-only line",
	"\tif err != nil {\n\t\treturn err\n\t}",
	"  \ta\n  b",
	"\n\n\tfirst\nsecond",
	"    code\n    more",
	"  a\n    b",
	"a\n\n\n",
	"\n\n",
	"   ",
	"",
}

// The bodies the syntax cannot carry at all: a first line indented deeper
// than a later one.
var unwritablePromptBodies = []string{
	"    a\n  b",
	"    Given:\n  - a\n  - b",
	"        first\n    less\n        deep",
	"  \ta\nb",
}

// indentBody writes body under a `prompt p:` header the way an author
// would: two spaces before every line, a blank or space-only line left
// blank. For a canonical body (no blank line, no trailing CR) that is
// exactly what writePrompts emits. (A tab-only line is indented like any
// other: for the lexer only spaces make a line blank, and that is what
// the writer relies on.)
func indentBody(body string) string {
	var sb strings.Builder
	sb.WriteString("prompt p:\n")
	for _, line := range strings.Split(body, "\n") {
		if strings.Trim(line, " ") == "" {
			sb.WriteString("\n")
			continue
		}
		sb.WriteString("  ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return sb.String()
}

// parseBody parses text and returns the body of its one prompt and any
// diagnostics.
func parseBody(t *testing.T, text string) (string, []Diagnostic) {
	t.Helper()
	pr := Parse("p.bot", text)
	if len(pr.File.Prompts) != 1 {
		t.Fatalf("%q: want one prompt, got %d", text, len(pr.File.Prompts))
	}
	return pr.File.Prompts[0].Body, pr.Diagnostics
}

// Where the lexer settles is the canonical form: the raw body written and
// read back — twice, since a trailing CR is folded one per pass — lands on
// CanonicalPromptBody; the canonical form written reads back unchanged;
// and the form is a fixed point of the function itself.
func TestCanonicalPromptBodyIsWhereTheLexerSettles(t *testing.T) {
	for _, body := range promptBodyShapes {
		if err := CheckPromptBody(body); err != nil {
			t.Errorf("%q: a writable body was refused: %v", body, err)
			continue
		}
		want := CanonicalPromptBody(body)
		if again := CanonicalPromptBody(want); again != want {
			t.Errorf("%q: the canonical form is not a fixed point: %q -> %q", body, want, again)
		}
		once, diags := parseBody(t, indentBody(body))
		for _, d := range diags {
			t.Errorf("%q: written raw, unexpected diagnostic: %s", body, d.Error())
		}
		twice, diags := parseBody(t, indentBody(once))
		for _, d := range diags {
			t.Errorf("%q: written after one pass, unexpected diagnostic: %s", body, d.Error())
		}
		if twice != want {
			t.Errorf("%q: the lexer settles on %q, CanonicalPromptBody says %q", body, twice, want)
		}
		got, diags := parseBody(t, indentBody(want))
		for _, d := range diags {
			t.Errorf("%q: the canonical form does not parse: %s", body, d.Error())
		}
		if got != want {
			t.Errorf("%q: the canonical form %q reads back as %q", body, want, got)
		}
	}
}

// A body the syntax cannot carry is named by CheckPromptBody — and the
// lexer indeed does not carry its written form: it refuses it, or reads a
// different body.
func TestCheckPromptBodyNamesWhatTheLexerCannotCarry(t *testing.T) {
	for _, body := range unwritablePromptBodies {
		err := CheckPromptBody(body)
		if err == nil {
			t.Errorf("%q: CheckPromptBody accepted a body the lexer cannot carry", body)
			continue
		}
		if !strings.Contains(err.Error(), "indented less") {
			t.Errorf("%q: the refusal does not say why: %v", body, err)
		}
		if got, diags := parseBody(t, indentBody(body)); len(diags) == 0 && got == body {
			t.Errorf("%q: the lexer carried a body CheckPromptBody refuses", body)
		}
	}
}
