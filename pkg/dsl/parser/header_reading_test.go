package parser

import (
	"strings"
	"testing"
)

// The `dsl:` header has two readers — the preamble, which decides how the
// lexer reads every string before tokenising, and the parser, which stamps
// the AST — and they must agree on every spelling, or a file gets one
// profile in its strings and another in its AST with nothing said. A header
// followed by anything but a comment is refused (E040) and read as profile 1
// by both; a profile this build does not read is refused and read as 1 by
// both; a trailing comment is fine for both.
func TestTheHeaderIsReadAlikeByTheLexerAndTheParser(t *testing.T) {
	body := "\ntool t:\n  command: \"a\\nb\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	cases := []struct {
		name    string
		header  string
		profile int      // what both readers must agree on
		code    DiagCode // the refusal, "" when the header is accepted
	}{
		{"plain", "dsl: 2", 2, ""},
		{"no space", "dsl:2", 2, ""},
		{"space before the colon", "dsl : 2", 2, ""},
		{"trailing comment", "dsl: 2 ## the profile", 2, ""},
		{"trailing single-hash comment", "dsl: 2 # p", 2, ""},
		{"tab before the comment", "dsl: 2\t## p", 2, ""},
		{"leading zero", "dsl: 02", 2, ""},
		{"explicit one", "dsl: 1", 1, ""},
		{"a word after the number", "dsl: 2 oops", 1, DiagUnknownProfile},
		{"a second number", "dsl: 2 2", 1, DiagUnknownProfile},
		// A character the lexer refuses is the lexer's diagnosis (E001),
		// whatever the parser wanted there — and the header is not stamped.
		{"a semicolon", "dsl: 2;", 1, DiagUnexpectedToken},
		{"a comma", "dsl: 2,", 1, DiagUnknownProfile},
		{"a sign", "dsl: +2", 1, DiagUnknownProfile},
		{"a future profile", "dsl: 3", 1, DiagUnknownProfile},
		{"a word", "dsl: two", 1, DiagUnknownProfile},
		{"quoted", "dsl: \"2\"", 1, DiagUnknownProfile},
		{"nothing", "dsl:", 1, DiagUnknownProfile},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := c.header + "\n" + body
			lex := NewLexer("x.bot", src).Profile()
			res := Parse("x.bot", src)
			if res.File.EffectiveProfile() != lex {
				t.Fatalf("%q: the parser reads profile %d, the lexer %d", c.header, res.File.EffectiveProfile(), lex)
			}
			if lex != c.profile {
				t.Fatalf("%q: read as profile %d, want %d", c.header, lex, c.profile)
			}
			var got DiagCode
			for _, d := range res.Diagnostics {
				if d.Severity == SeverityError {
					got = d.Code
					break
				}
			}
			if got != c.code {
				t.Fatalf("%q: first error %q, want %q (%v)", c.header, got, c.code, res.Diagnostics)
			}
			// The one observable that matters: the string's reading follows
			// the profile both agreed on.
			want := "a\nb"
			if c.profile == 1 {
				want = `a\nb`
			}
			if got := res.File.Tools[0].Command; got != want {
				t.Fatalf("%q: command reads %q, want %q", c.header, got, want)
			}
		})
	}
}

// The refusal of junk after the number names what was read.
func TestJunkAfterTheProfileNumberIsNamed(t *testing.T) {
	res := Parse("x.bot", "dsl: 2 oops\nagent a:\n  description: \"x\"\n")
	var msg string
	for _, d := range res.Diagnostics {
		if d.Code == DiagUnknownProfile {
			msg = d.Message
		}
	}
	if !strings.Contains(msg, "only the profile number") || !strings.Contains(msg, "oops") {
		t.Fatalf("E040 = %q", msg)
	}
	if res.File.Profile != 0 {
		t.Fatalf("the profile was stamped despite the refusal: %d", res.File.Profile)
	}
}

// Under a header this build refuses, the file's strings, prompt bodies and
// removed properties read as profile 1 — not as the future profile's.
func TestARefusedFutureProfileReadsAsProfileOne(t *testing.T) {
	src := "dsl: 3\n\nprompt p:\n  One.\n\n  Two.\n\nagent a:\n  system: p\n  description: \"a\\nb\"\n  memory:\n    enabled: true\n    project_root: true\n\nworkflow w:\n  entry: a\n  a -> done\n"
	res := Parse("x.bot", src)
	codes := map[DiagCode]int{}
	for _, d := range res.Diagnostics {
		codes[d.Code]++
	}
	if codes[DiagUnknownProfile] != 1 || codes[DiagRemovedInProfile] != 0 {
		t.Fatalf("diagnostics: %v", res.Diagnostics)
	}
	if got := res.File.Agents[0].Description; got != `a\nb` {
		t.Fatalf("the string was read with the future profile's escapes: %q", got)
	}
	if got := res.File.Prompts[0].Body; strings.Contains(got, "\n\n") {
		t.Fatalf("the prompt body kept its blank line under a refused header: %q", got)
	}
}
