package parser

import (
	"strings"
	"testing"
)

// A .bot holding a byte that is not UTF-8 is refused with the byte's line
// (#1808): the lexer reads runes, so the byte would arrive everywhere as
// U+FFFD where its author wrote another character, and the parser would
// then report misleading errors at positions the author never wrote —
// measured: E002 "expected workflow name" at 1:10 for a café written in
// Latin-1. The author document refuses the same byte (E050); the .bot does
// so from here.
func TestParseRefusesNonUTF8Source(t *testing.T) {
	src := "workflow w:\n  entry: a\n\nagent a:\n  backend: \"claude_code\"\n  system: \"caf\xe9\"\n"
	res := Parse("latin1.bot", src)
	if len(res.Diagnostics) != 1 {
		t.Fatalf("diagnostics: %v, want exactly the E006 refusal", res.Diagnostics)
	}
	d := res.Diagnostics[0]
	if d.Code != DiagNotUTF8 || d.Severity != SeverityError {
		t.Fatalf("diagnostic: %+v, want an E006 error", d)
	}
	if d.Line != 6 {
		t.Fatalf("line = %d, want 6 (the byte's line)", d.Line)
	}
	if d.Column != 1 {
		t.Fatalf("column = %d, want 1", d.Column)
	}
	if !strings.Contains(d.Message, "byte ") {
		t.Fatalf("message names no byte offset: %q", d.Message)
	}
	if res.File != nil {
		t.Fatal("a refused source returns no AST: callers check the diagnostics first, and a partial AST invites compiling garbage")
	}
}

// A source whose every byte is UTF-8 — accents included — parses as before:
// the guard refuses a byte that is not UTF-8, never an accent.
func TestParseAcceptsUTF8Accents(t *testing.T) {
	src := "workflow w:\n  entry: a\n\nagent a:\n  backend: \"claude_code\"\n  system: \"café — naïve\"\n"
	res := Parse("utf8.bot", src)
	for _, d := range res.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("valid UTF-8 refused: %v", d)
		}
	}
	if res.File == nil {
		t.Fatal("no AST for a valid UTF-8 source")
	}
}
