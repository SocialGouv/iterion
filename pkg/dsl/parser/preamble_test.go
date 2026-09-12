package parser

import (
	"strings"
	"testing"
)

// The head of a file is read by ONE reader — the lexer's escape mode, the
// unparser's head and the migrator all ask it — and it finds the `dsl:`
// header on the first significant line, past blank lines, comments and the
// frontmatter block, however many of those there are.
func TestReadPreambleFindsTheHeaderOnTheFirstSignificantLine(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		profile int
		line    int
	}{
		{"absent", "agent a:\n  description: \"x\"\n", 0, 0},
		{"first line", "dsl: 2\nagent a:\n", 2, 1},
		{"no space", "dsl:2\n", 2, 1},
		{"trailing comment", "dsl: 2  ## the profile\n", 2, 1},
		{"after blank lines and comments", "\n## a bot\n\n# another note\ndsl: 2\n", 2, 5},
		{"after the frontmatter block", "## ---\n## name: x\n## ---\ndsl: 2\n", 2, 4},
		{"after forty comment lines", strings.Repeat("## c\n", 40) + "dsl: 2\n", 2, 41},
		{"explicit profile 1", "dsl: 1\n", 1, 1},
		{"not first: a declaration precedes it", "vars:\n  x: string\ndsl: 2\n", 0, 0},
		{"indented is not a header", "  dsl: 2\n", 0, 0},
		{"a node named dsl is not a header", "agent dsl:\n  description: \"x\"\n", 0, 0},
		{"not an integer", "dsl: two\n", -1, 1},
		{"zero", "dsl: 0\n", -1, 1},
		{"quoted", "dsl: \"2\"\n", -1, 1},
		{"empty", "dsl:\n", -1, 1},
		{"last line without newline", "## c\ndsl: 2", 2, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pre := ReadPreamble(c.src)
			if pre.Profile != c.profile || pre.HeaderLine != c.line {
				t.Fatalf("ReadPreamble(%q) = profile %d line %d, want profile %d line %d", c.src, pre.Profile, pre.HeaderLine, c.profile, c.line)
			}
		})
	}
}

// The profile-1 directive keeps its own rule, FROZEN: among the first 32
// lines, before the first non-comment line. Reading it further down would
// change what a valid headerless file means — a string with a backslash
// under a directive on line 33 is read verbatim today, and stays so.
func TestReadPreambleKeepsTheDirectiveRuleOfProfileOne(t *testing.T) {
	within := strings.Repeat("## c\n", 30) + "## strict-escape: on\n" + "tool t:\n  command: \"a\\nb\"\n"
	if pre := ReadPreamble(within); !pre.StrictEscape {
		t.Fatalf("a directive on line 31 is read")
	}
	beyond := strings.Repeat("## c\n", 32) + "## strict-escape: on\n" + "tool t:\n  command: \"a\\nb\"\n"
	if pre := ReadPreamble(beyond); pre.StrictEscape {
		t.Fatalf("a directive on line 33 is NOT read — profile 1's rule is frozen")
	}
	// And the lexer reads that string exactly as before.
	res := Parse("x.bot", beyond)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	if got := res.File.Tools[0].Command; got != `a\nb` {
		t.Fatalf("command read as %q, want the verbatim backslash-n of profile 1", got)
	}
	// A directive after a non-comment line is not read either.
	late := "tool t:\n  command: \"x\"\n## strict-escape: on\n"
	if pre := ReadPreamble(late); pre.StrictEscape {
		t.Fatalf("a directive after the first line of code is not read")
	}
}

// The header sets the file's profile, and the profile decides the escape
// mode before the first string is tokenised: under `dsl: 2` a quoted
// string reads standard escapes with no directive.
func TestDSLHeaderSetsTheProfileAndTheEscapeMode(t *testing.T) {
	src := "dsl: 2\n\ntool t:\n  command: \"a\\nb \\\"q\\\"\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	res := Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	if res.File.Profile != 2 || res.File.EffectiveProfile() != 2 {
		t.Fatalf("profile = %d, want 2", res.File.Profile)
	}
	if got := res.File.Tools[0].Command; got != "a\nb \"q\"" {
		t.Fatalf("command read as %q, want standard escapes under profile 2", got)
	}
	// Without a header the same file is profile 1 and keeps the backslashes.
	v1 := Parse("x.bot", strings.TrimPrefix(src, "dsl: 2\n"))
	if v1.File.Profile != 0 || v1.File.EffectiveProfile() != 1 {
		t.Fatalf("headerless profile = %d (effective %d), want 0 (effective 1)", v1.File.Profile, v1.File.EffectiveProfile())
	}
	if got := v1.File.Tools[0].Command; got != `a\nb \"q\"` {
		t.Fatalf("command read as %q, want the verbatim escapes of profile 1", got)
	}
	// An explicit `dsl: 1` is profile 1.
	if r := Parse("x.bot", "dsl: 1\n"+strings.TrimPrefix(src, "dsl: 2\n")); r.File.Profile != 1 || len(r.Diagnostics) != 0 {
		t.Fatalf("explicit dsl: 1 → profile %d, diagnostics %v", r.File.Profile, r.Diagnostics)
	}
}

// A header this build cannot honour is refused by name, never guessed:
// an unknown profile (a newer engine's), a value that is not a positive
// integer, a header that is not the first declaration, a second header.
// Every refusal reads the file as profile 1, which is what the lexer did.
func TestDSLHeaderRefusals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code DiagCode
		msg  string
	}{
		{"newer profile", "dsl: 3\nagent a:\n  description: \"x\"\n", DiagUnknownProfile, "unknown dsl profile 3 — this build reads profiles 1 to 2"},
		{"not an integer", "dsl: two\n", DiagUnknownProfile, "got 'two'"},
		{"zero", "dsl: 0\n", DiagUnknownProfile, "got '0'"},
		{"quoted", "dsl: \"2\"\n", DiagUnknownProfile, "got '2'"},
		{"nothing", "dsl:\nagent a:\n  description: \"x\"\n", DiagUnknownProfile, "got nothing"},
		{"after a declaration", "vars:\n  x: string\ndsl: 2\n", DiagMisplacedHeader, "must be the first declaration"},
		{"twice", "dsl: 2\ndsl: 2\n", DiagMisplacedHeader, "duplicate dsl: header"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", c.src)
			var found *Diagnostic
			for i := range res.Diagnostics {
				if res.Diagnostics[i].Code == c.code {
					found = &res.Diagnostics[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("no %s diagnostic in %v", c.code, res.Diagnostics)
			}
			if !strings.Contains(found.Message, c.msg) {
				t.Fatalf("%s message %q does not carry %q", c.code, found.Message, c.msg)
			}
			if found.Hint == "" {
				t.Fatalf("%s arrives without a remedy", c.code)
			}
			if res.File.Profile != 0 && c.name != "twice" {
				t.Fatalf("a refused header set the profile to %d", res.File.Profile)
			}
			// One diagnostic for the header, not a cascade over the line.
			n := 0
			for _, d := range res.Diagnostics {
				if d.Line == found.Line {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d diagnostics on the header line, want 1: %v", n, res.Diagnostics)
			}
		})
	}
	// The unknown-profile remedy names the manifest floor that keeps the
	// file off an older build.
	res := Parse("x.bot", "dsl: 3\n")
	if !strings.Contains(res.Diagnostics[0].Hint, "requires") {
		t.Fatalf("E040 hint does not name requires.iterion: %q", res.Diagnostics[0].Hint)
	}
}

// `dsl` is a keyword so the header dispatches like every top-level form,
// and like every keyword it stays a valid name: a node, a field, an edge
// endpoint.
func TestDSLIsAKeywordAndStillAName(t *testing.T) {
	src := "dsl: 2\n\nschema s:\n  dsl: string\n\nagent dsl:\n  description: \"x\"\n\nworkflow w:\n  entry: dsl\n  dsl -> done\n"
	res := Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	if res.File.Agents[0].Name != "dsl" || res.File.Workflows[0].Edges[0].From != "dsl" {
		t.Fatalf("dsl as a name did not survive: %+v", res.File.Workflows[0].Edges)
	}
}
