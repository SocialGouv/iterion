package parser_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// One bad escape is ONE diagnostic: the lexer consumes the rest of the
// literal, so the bytes it still held do not each become an "unexpected
// character" the body loop then reads as a property name.
func TestBadEscapeIsReportedOnce(t *testing.T) {
	pr := parser.Parse("esc.bot", "# strict-escape: on\nagent a:\n  model: \"a\\db\"\n  output: out\n")
	if len(pr.Diagnostics) != 1 || pr.Diagnostics[0].Code != parser.DiagBadEscape {
		var got []string
		for _, d := range pr.Diagnostics {
			got = append(got, d.Error())
		}
		t.Fatalf("want exactly one E005, got:\n%s", strings.Join(got, "\n"))
	}
}

// An indented line with no open block above it (its header failed) used to
// read "unexpected token '' at top level" — an indent token has no text.
func TestIndentOutsideAnyBlockIsNamed(t *testing.T) {
	pr := parser.Parse("ind.bot", "agent a:\n\tmodel: \"m\"\n  output: out\n")
	var named bool
	for _, d := range pr.Diagnostics {
		if strings.Contains(d.Message, "unexpected token ''") {
			t.Errorf("opaque message survived: %s", d.Error())
		}
		if d.Code == parser.DiagBadIndentation && strings.Contains(d.Message, "indented line outside any block") && d.Line == 3 {
			named = true
		}
	}
	if !named {
		var got []string
		for _, d := range pr.Diagnostics {
			got = append(got, d.Error())
		}
		t.Errorf("line 3 not reported as an indented line outside any block:\n%s", strings.Join(got, "\n"))
	}
}

// A diagnosis the lexer makes (a tab, an unterminated string, a bad escape)
// must reach the author as what it is — its own code, message and fix line —
// wherever the error token surfaces. Inside a declaration body it used to
// arrive as "expected INDENT, got Error" with a hint about opening a block
// the author had opened: a hint that produces a WORSE file.
func TestLexerDiagnosisSurvivesInsideBlocks(t *testing.T) {
	cases := []struct {
		name, src string
		code      parser.DiagCode
		message   string
	}{
		{"tab inside a node body", "agent a:\n\tmodel: \"m\"\n", parser.DiagBadIndentation, "tabs are not allowed"},
		{"tab at top level", "\tagent a:\n", parser.DiagBadIndentation, "tabs are not allowed"},
		{"unterminated string in a property", "agent a:\n  model: \"m\n  output: out\n", parser.DiagUnterminatedStr, "unterminated string literal"},
		{"unterminated raw string", "tool t:\n  command: `echo hi\n", parser.DiagUnterminatedStr, "unterminated raw string"},
		{"unknown escape under strict-escape", "# strict-escape: on\nagent a:\n  model: \"a\\db\"\n", parser.DiagBadEscape, "unknown escape sequence"},
		{"stray character in a node body", "agent a:\n  model: @\n", parser.DiagUnexpectedToken, "unexpected character"},
		{"stray character at a property position", "agent a:\n  @foo: 1\n", parser.DiagUnexpectedToken, "unexpected character"},
		{"stray character at a router property position", "router r:\n  @mode: llm\n", parser.DiagUnexpectedToken, "unexpected character"},
		{"misaligned dedent", "agent a:\n    model: \"m\"\n  output: out\n", parser.DiagBadIndentation, "does not match any outer level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := parser.Parse("lex.bot", tc.src)
			var found bool
			for _, d := range pr.Diagnostics {
				if d.Code == tc.code && strings.Contains(d.Message, tc.message) {
					found = true
					if d.Hint == "" || d.Hint != parser.HintFor(tc.code) {
						t.Errorf("hint = %q, want the %s catalogue line %q", d.Hint, tc.code, parser.HintFor(tc.code))
					}
				}
				if strings.Contains(d.Message, "got Error") {
					t.Errorf("the lexer's diagnosis was thrown away: %s | fix: %s", d.Error(), d.Hint)
				}
				if d.Code == parser.DiagUnknownProperty && strings.Contains(d.Message, "unexpected character") {
					t.Errorf("the lexer's diagnosis was read as a property name: %s", d.Error())
				}
			}
			if !found {
				var got []string
				for _, d := range pr.Diagnostics {
					got = append(got, d.Error()+" | fix: "+d.Hint)
				}
				t.Errorf("no %s containing %q; diagnostics:\n%s", tc.code, tc.message, strings.Join(got, "\n"))
			}
		})
	}
}
