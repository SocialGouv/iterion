package author

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// lineOf is the 1-based line of the first line of src that starts with
// prefix (leading spaces included), 0 when none does.
func lineOf(src, prefix string) int {
	for i, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(l, prefix) {
			return i + 1
		}
	}
	return 0
}

// Every declaration's position is the YAML line it was written on — not
// the line of the text the parser read — so a diagnostic, a canvas or a
// save points at the author's own file.
func TestDeclarationPositionsAreTheYAMLLines(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(fixtureDir("valid"), "every-kind-v2.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res := Parse("every-kind-v2.yaml", src)
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("refused: %v", errs)
	}
	text := string(src)
	f := res.File
	for _, tc := range []struct {
		what   string
		prefix string
		got    int
		file   string
	}{
		{"agent survey", "  - agent: survey", f.Agents[0].Span.Start.Line, f.Agents[0].Span.Start.File},
		{"tool comment", "  - tool: comment", f.Tools[2].Span.Start.Line, f.Tools[2].Span.Start.File},
		{"fail rejected", "  - fail: rejected", f.Fails[0].Span.Start.Line, f.Fails[0].Span.Start.File},
		{"the workflow", "  name: release_readiness", f.Workflows[0].Span.Start.Line, f.Workflows[0].Span.Start.File},
		{"prompt reviewer", "  reviewer: |", f.Prompts[0].Span.Start.Line, f.Prompts[0].Span.Start.File},
		{"var goal", "  goal: string", f.Vars.Fields[0].Span.Start.Line, f.Vars.Fields[0].Span.Start.File},
		{"the group review", "  - group: review", f.Groups[0].Span.Start.Line, f.Groups[0].Span.Start.File},
		{"the group's agent look", "      - agent: look", f.Groups[0].Agents[0].Span.Start.Line, f.Groups[0].Agents[0].Span.Start.File},
		{"the first edge", "    - survey -> split", f.Workflows[0].Edges[0].Span.Start.Line, f.Workflows[0].Edges[0].Span.Start.File},
		{"the contract public", "  public:", f.Contracts[0].Span.Start.Line, f.Contracts[0].Span.Start.File},
		{"the fallback route spare", "      - route: spare", f.Agents[0].Fallbacks[0].Span.Start.Line, f.Agents[0].Fallbacks[0].Span.Start.File},
	} {
		want := lineOf(text, tc.prefix)
		if want == 0 {
			t.Fatalf("%s: the fixture has no line starting with %q", tc.what, tc.prefix)
		}
		if tc.got != want {
			t.Errorf("%s: positioned at line %d, written at line %d", tc.what, tc.got, want)
		}
		if tc.file != "every-kind-v2.yaml" {
			t.Errorf("%s: positioned in %q, want the document's name", tc.what, tc.file)
		}
	}
}

// A diagnostic — the converter's, the parser's or the compiler's — points
// at the YAML line and column of what it names.
func TestDiagnosticsPointAtTheYAML(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		code    parser.DiagCode
		prefix  string // the line the diagnostic names
		col     int    // its column, 0 for any
	}{
		{"unknown-property.yaml", parser.DiagUnknownProperty, "    modle: m", 5},
		{"wrong-type-int.yaml", parser.DiagAuthorValue, "    max_tokens: many", 17},
		{"enum-outside-list.yaml", parser.DiagInvalidValue, "    session: sometimes", 14},
		{"out-of-profile.yaml", parser.DiagRemovedInProfile, "      project_root: true", 7},
		{"duplicate-key.yaml", parser.DiagAuthorDocument, "    model: n", 5},
		{"negative-int.yaml", parser.DiagAuthorValue, "    max_tokens: -1", 17},
		{"schema-field-file-in-var.yaml", parser.DiagInvalidType, "  x: file", 6},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			path := filepath.Join(fixtureDir("invalid"), tc.fixture)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			res := Parse(tc.fixture, src)
			want := lineOf(string(src), tc.prefix)
			if want == 0 {
				t.Fatalf("the fixture has no line starting with %q", tc.prefix)
			}
			for _, d := range res.Diagnostics {
				if d.Code != tc.code {
					continue
				}
				if d.File != tc.fixture {
					t.Errorf("%s is positioned in %q, want the document's name", d.Code, d.File)
				}
				if d.Line != want || (tc.col != 0 && d.Column != tc.col) {
					t.Errorf("%s is positioned at %d:%d, want %d:%d — %s", d.Code, d.Line, d.Column, want, tc.col, d.Message)
				}
				return
			}
			t.Fatalf("no %s among the diagnostics: %v\n--- spelled as:\n%s", tc.code, res.Diagnostics, res.Text)
		})
	}
}

// A diagnostic on an edge line points at the character in the YAML string
// when the document's text carries the string verbatim on one line — plain
// or quoted, spaces inside the quotes included — and at the string's own
// start when it does not (a YAML escape shrank the value, a plain scalar is
// folded over two lines): never a column the author did not write, never
// one past the end of a line. The wanted position is counted on the SOURCE,
// never derived from the converter's arithmetic.
func TestEdgeDiagnosticsPointIntoTheString(t *testing.T) {
	const head = "dsl: 2\nnodes:\n  - agent: a\n    model: m\n  - agent: b\n    model: m\nworkflow:\n  name: w\n  entry: a\n  edges:\n"
	for _, tc := range []struct {
		name  string
		lines string // the edge item, one or two physical lines
		want  string // "exact": the `&` of the first line; "start": the scalar's start
	}{
		{"plain", "    - a -> b when x && y", "exact"},
		{"double-quoted", `    - "a -> b when x && y"`, "exact"},
		{"single-quoted", `    - 'a -> b when x && y'`, "exact"},
		{"quoted with leading spaces", `    - "   a -> b when x && y"`, "exact"},
		{"quoted with an escape before the fault", `    - "a -> \x41 when x && y"`, "start"},
		{"plain folded over two lines", "    - a -> b\n        when x && y", "start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := head + tc.lines + "\n"
			res := Parse("e.yaml", []byte(doc))
			first := strings.SplitN(tc.lines, "\n", 2)[0]
			wantCol := strings.Index(first, "&") + 1 // 1-based column of the `&` on the first line
			if tc.want == "start" {
				wantCol = strings.Index(first, "- ") + 3 // the scalar starts after "- "
			}
			for _, d := range res.Diagnostics {
				if d.Code != parser.DiagUnexpectedToken {
					continue
				}
				if d.Line != 11 || d.Column != wantCol {
					t.Errorf("E001 at %d:%d, want 11:%d (%s)", d.Line, d.Column, wantCol, tc.want)
				}
				if d.Column > len(first) {
					t.Errorf("E001 at column %d, past the end of line 11 (%d characters)", d.Column, len(first))
				}
				if !strings.Contains(d.Message, "quoted expression") {
					t.Errorf("the message does not name the quoted form: %s", d.Message)
				}
				return
			}
			t.Fatalf("no E001 among %v", res.Diagnostics)
		})
	}
}

// The compiler's diagnostics on a program read from YAML carry the YAML's
// name and the YAML line of what they name, wherever the compiler gives
// the same program's .bot a position: an edge to a node the graph lacks is
// reported on the edge's own line of the document.
func TestCompileDiagnosticsCarryTheYAMLPosition(t *testing.T) {
	const doc = `dsl: 2
nodes:
  - agent: a
    model: m
workflow:
  name: w
  entry: a
  edges:
    - a -> nowhere
`
	res := Parse("w.bot.yaml", []byte(doc))
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("refused: %v", errs)
	}
	bot := parser.Parse("w.bot", "dsl: 2\n\nagent a:\n  model: m\n\nworkflow w:\n  entry: a\n  a -> nowhere\n")
	var fromBot *ir.Diagnostic
	for _, d := range ir.Compile(bot.File).Diagnostics {
		if d.Code == ir.DiagUnknownNode {
			d := d
			fromBot = &d
			break
		}
	}
	if fromBot == nil || fromBot.Line == 0 {
		t.Fatalf("the .bot's own C001 carries no position (%v): the test has nothing to hold the twin to", fromBot)
	}
	for _, d := range ir.Compile(res.File).Diagnostics {
		if d.Code != ir.DiagUnknownNode {
			continue
		}
		if d.File != "w.bot.yaml" || d.Line != lineOf(doc, "    - a -> nowhere") {
			t.Errorf("C001 positioned at %s:%d:%d, want w.bot.yaml:%d (the edge's line)", d.File, d.Line, d.Column, lineOf(doc, "    - a -> nowhere"))
		}
		return
	}
	t.Fatalf("no C001 among the twin's diagnostics: %v", ir.Compile(res.File).Diagnostics)
}
