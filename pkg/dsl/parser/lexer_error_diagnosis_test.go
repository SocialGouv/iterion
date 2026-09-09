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

// An indented line where no member or property can be used to read
// "unexpected token ” at top level", "unexpected token ” in workflow" or
// "unknown agent property ”" depending on the block it fell into — an
// indent token has no text. One message, from the one emitter, at all three.
func TestIndentOutsideAnyBlockIsNamed(t *testing.T) {
	cases := []struct {
		name, src string
		line      int
	}{
		{"top level after a failed header", "agent a:\n\tmodel: \"m\"\n  output: out\n", 3},
		{"workflow body", "schema out:\n  ok: bool\nagent a:\n  model: \"m\"\n  output: out\nworkflow w:\n  entry: a\n    a -> done\n", 8},
		{"node body", "agent a:\n  model: \"m\"\n    output: out\n", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := parser.Parse("ind.bot", tc.src)
			var named bool
			for _, d := range pr.Diagnostics {
				if strings.Contains(d.Message, "''") {
					t.Errorf("opaque empty-name message survived: %s", d.Error())
				}
				if d.Code == parser.DiagBadIndentation && strings.Contains(d.Message, "indented line where none can be") && d.Line == tc.line {
					named = true
				}
			}
			if !named {
				var got []string
				for _, d := range pr.Diagnostics {
					got = append(got, d.Error())
				}
				t.Errorf("line %d not reported as an indented line where none can be:\n%s", tc.line, strings.Join(got, "\n"))
			}
		})
	}
}

// A trailing comment stands in for that line's newline in the token stream,
// so the recovery after an unknown property must stop at it — it used to run
// into the next line and swallow the `expr:` header, which the compiler then
// reported as "compute has no expr block" on a compute that has one.
func TestRecoveryAfterUnknownPropertyStopsAtTrailingComment(t *testing.T) {
	pr := parser.Parse("cmt.bot", "schema s:\n  kind: string\n\ncompute big:\n  output: s\n  bogus: 1 # c\n  expr:\n    kind: \"1\"\n")
	if pr.File == nil || len(pr.File.Computes) != 1 {
		t.Fatalf("expected one compute, got %+v", pr.File)
	}
	if len(pr.File.Computes[0].Expr) != 1 {
		var got []string
		for _, d := range pr.Diagnostics {
			got = append(got, d.Error())
		}
		t.Errorf("the expr block after the commented line was lost; diagnostics:\n%s", strings.Join(got, "\n"))
	}
	if n := len(pr.Diagnostics); n != 1 || pr.Diagnostics[0].Code != parser.DiagUnknownProperty {
		var got []string
		for _, d := range pr.Diagnostics {
			got = append(got, d.Error())
		}
		t.Errorf("want exactly one E012 (bogus), got %d:\n%s", n, strings.Join(got, "\n"))
	}
}

// The lexer's diagnosis is not a value: a bad escape where a prompt name
// belongs must not become the prompt name the compiler then reports as
// unknown.
func TestLexerDiagnosisIsNotAValue(t *testing.T) {
	pr := parser.Parse("val.bot", "# strict-escape: on\nagent a:\n  system: \"a\\db\"\n  model: \"m\"\n")
	if pr.File == nil || len(pr.File.Agents) != 1 {
		t.Fatalf("expected one agent, got %+v", pr.File)
	}
	if got := pr.File.Agents[0].System; got != "" {
		t.Errorf("System = %q, want empty (the lexer's diagnosis is not a name)", got)
	}
	if got := pr.File.Agents[0].Model; got != "m" {
		t.Errorf("Model = %q: the recovery after the bad escape lost the next property", got)
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
		// The block-scalar opener's diagnosis shares the parser's E002 code:
		// the replacement must not depend on the codes differing.
		{"text after a block scalar opener", "tool t:\n  command: |x\n  output: out\n", parser.DiagExpectedToken, "expected newline after '|'"},
		{"block scalar opener before a list", "agent a:\n  capabilities: |[board.create]\n", parser.DiagExpectedToken, "expected newline after '|'"},
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
