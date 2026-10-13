package author

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A prompt body the lexer settles otherwise than written reads without an
// error, and the reading is SAID: a warning names what is dropped, at the
// body's line; the document still has no error.
func TestAPromptBodyTheLexerSettlesIsWarned(t *testing.T) {
	path := filepath.Join(fixtureDir("valid"), "prompt-body-settled.yaml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	res := Parse("settled.yaml", src)
	if res.HasErrors() {
		t.Fatalf("the document is refused: %v", res.Diagnostics)
	}
	var warned *parser.Diagnostic
	for i, d := range res.Diagnostics {
		if d.Code == parser.DiagAuthorPromptBody && d.Severity == parser.SeverityWarning {
			warned = &res.Diagnostics[i]
		}
	}
	if warned == nil {
		t.Fatalf("no E053 warning among %v", res.Diagnostics)
	}
	if want := lineOf(string(src), "  spaced: |"); warned.Line != want {
		t.Errorf("the warning is at line %d, want %d (the body's)", warned.Line, want)
	}
	if !strings.Contains(warned.Message, "leading blank line") {
		t.Errorf("the warning does not name the dropped lines: %s", warned.Message)
	}
	if body := res.File.Prompts[0].Body; body != "Indented and surrounded by blank lines.\nSecond line." {
		t.Errorf("the body read is %q, not the settled form", body)
	}
}

// A YAML syntax error arrives with the spelling that avoids it, not only
// the scanner's own words: a plain value holding `: ` — the trap a shell
// command with a jq filter falls into — says "quote the whole value".
func TestAYAMLSyntaxErrorNamesItsRemedy(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(fixtureDir("invalid"), "plain-value-holds-a-colon.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res := Parse("colon.yaml", src)
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != parser.DiagAuthorDocument {
		t.Fatalf("want one E050, got %v", res.Diagnostics)
	}
	d := res.Diagnostics[0]
	if d.Line != lineOf(string(src), "    command: go test") {
		t.Errorf("positioned at line %d, want the command's line", d.Line)
	}
	if !strings.Contains(d.Message, "quote the whole value") {
		t.Errorf("the message does not name the remedy: %s", d.Message)
	}
}

// A scanner message given for two different mistakes names the remedy of
// each: over-indentation reads as a plain value holding `: `, and a value
// with quotes of its own — the edge whose with map holds a quoted value,
// the trap the documentation names — reads as a key missing, like an
// indentation mistake does.
func TestAYAMLSyntaxErrorNamesTheRemedyOfEachOfItsCauses(t *testing.T) {
	tail := "workflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n"
	edges := func(edge string) string {
		return "dsl: 2\nnodes:\n  - agent: a\n    model: m\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - " + edge + "\n"
	}
	for name, tc := range map[string]struct{ src, want string }{
		"a key indented too deep":               {"dsl: 2\nnodes:\n  - agent: a\n     model: m\n" + tail, "indented deeper"},
		"a key indented too shallow":            {"dsl: 2\nnodes:\n  - agent: a\n model: m\n" + tail, "indentation:"},
		"an edge with a quoted value, plain":    {edges(`a -> done with { x: "{{outputs.a.y}}" }`), "single quotes"},
		"an edge in double quotes of its own":   {edges(`"a -> done with { x: "{{outputs.a.y}}" }"`), "single quotes"},
		"a command in double quotes of its own": {"dsl: 2\nnodes:\n  - tool: t\n    command: \"echo \"hi\"\"\n" + tail, "single quotes"},
	} {
		t.Run(name, func(t *testing.T) {
			res := Parse("x.bot.yaml", []byte(tc.src))
			if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != parser.DiagAuthorDocument {
				t.Fatalf("want one E050, got %v", res.Diagnostics)
			}
			if msg := res.Diagnostics[0].Message; !strings.Contains(msg, tc.want) {
				t.Fatalf("the message does not name the remedy %q: %s", tc.want, msg)
			}
		})
	}
}

// A second document refused for its own syntax lands on the line its words
// name: the decode that failed leaves the node unfilled, so the position
// comes from yaml.v3's error, as it does for the first document. The remedy
// tells the author to remove a `...` line or to close a value — an editor
// squiggle on line 1 points at neither.
func TestASecondDocumentRefusalLandsOnTheLineItNames(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		want int
	}{
		"a ... line with a document after it": {"dsl: 2\nnodes:\n  - agent: a\n    model: m\n...\nworkflow:\n  name: w\n  entry: a\n", 5},
		"a second document with a bad list":   {"dsl: 2\n---\nimports: - lib/x.bot\n", 3},
		"a second document with a flow open":  {"dsl: 2\nnodes:\n  - agent: a\n    model: m\n...\nimports: [a,\n", 5},
		// A first document that is a whole value of its own is the line the
		// remedy tells the author to edit, in each of the shapes it names:
		// yaml's own number is the line before the token that opened the
		// second document, which a comment or a blank line between the two
		// makes a line the author must not touch.
		"a stray first line above a comment":     {"my first bot\n# a note\n" + readsAloneHead + readsAloneBody, 1},
		"a stray first line above a blank line":  {"my first bot\n\n# a note\n" + readsAloneHead + readsAloneBody, 1},
		"a first key quoted whole above a note":  {`"dsl: 2"` + "\n# a note\n" + readsAloneBody, 1},
		"a flow list first line above a comment": {"[a, b]\n# a note\n" + readsAloneHead + readsAloneBody, 1},
		"a flow map first line above a comment":  {"{a: b}\n# a note\n" + readsAloneHead + readsAloneBody, 1},
		"a block scalar as the first line":       {"|\n  a title\n" + readsAloneHead + readsAloneBody, 1},
		"a first key written without its colon":  {"dsl 2\n# a note\nimports:\n  - lib/x.bot\n", 1},
		// An error of the SECOND document's own syntax keeps yaml's line,
		// whatever shape the first document has: the remedy that rides it
		// names that document's spelling, never the first line.
		"a bad list under a stray first line":    {"my first bot\n---\nimports: - lib/x.bot\n", 3},
		"a bad list five lines below a stray":    {"my first bot\n---\ndsl: 2\nnodes:\n  - agent: a\nimports: - x\n", 6},
		"a bad list under a flow-list first doc": {"[a, b]\n---\nimports: - lib/x.bot\n", 3},
		"an open flow under a quoted first key":  {`"dsl: 2"` + "\n---\nimports: [a,\n", 3},
		// A `...` closing the first document is itself the line that same
		// remedy names first — an indicator where it starts the line and a
		// blank follows the three dots, as yaml.v3's scanner reads it: the
		// body of a `|` block holding one is part of the value.
		"a ... closing a block sequence": {"- a\n- b\n...\ndsl: 2\n", 3},
		"a ... closing a flow map":       {"{a: b}\n...\ndsl: 2\n", 2},
		"a ... closing a plain value":    {"a title\n...\nimports: - lib/x.bot\n", 2},
		"a ... closing a block scalar":   {"|\n  a title\n...\ndsl: 2\n", 3},
		"a ... followed by a tab":        {"- a\n- b\n...\t\ndsl: 2\n", 3},
		"a ... followed by a comment":    {"- a\n- b\n... # a note\ndsl: 2\n", 3},
		"a ... indented inside a block":  {"|\n  ...\ndsl: 2\n", 1},
		// The `...` is read off the lines BETWEEN the first document and the
		// line yaml names, not off that one line: a comment or a blank
		// between the indicator and the second document's first token moves
		// yaml's number past it, and the line the remedy names first would
		// be nowhere in the refusal. The reading holds for a block mapping
		// first document too, where no node of ours carries the line.
		"a ... then a comment, plain value root":    {"a title\n...\n# a note\nimports: lib/x.bot\n", 2},
		"a ... then a blank line, plain value root": {"a title\n...\n\nimports: lib/x.bot\n", 2},
		"a ... then two comments, plain value root": {"a title\n...\n# one\n# two\nimports: lib/x.bot\n", 2},
		"a ... then a comment, flow list root":      {"[a, b]\n...\n# a note\nimports: lib/x.bot\n", 2},
		"a ... then a comment, block scalar root":   {"|\n  x\n...\n# a note\nimports: lib/x.bot\n", 3},
		"a ... then a comment, block mapping root":  {"dsl: 2\n...\n# a note\nimports: lib/x.bot\n", 2},
		"a ... then a blank, block mapping root":    {readsAloneHead + readsAloneBody + "...\n\nimports: lib/x.bot\n", 8},
		// The second document READS: the position is the node's own, which
		// the error cannot give — the branch the line above does not touch.
		"a second document that reads": {"dsl: 2\n---\nimports:\n  - lib/x.bot\n", 3},
	} {
		t.Run(name, func(t *testing.T) {
			res := Parse("x.bot.yaml", []byte(tc.src))
			if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != parser.DiagAuthorDocument {
				t.Fatalf("want one E050, got %v", res.Diagnostics)
			}
			d := res.Diagnostics[0]
			if d.Line != tc.want {
				t.Errorf("the refusal lands on line %d, want %d: %s", d.Line, tc.want, d.Message)
			}
			if m := yamlLineRe.FindStringSubmatch(d.Message); m != nil && m[1] != strconv.Itoa(d.Line) {
				t.Errorf("the refusal lands on line %d while its own words name line %s: %s", d.Line, m[1], d.Message)
			}
		})
	}
}

// HasErrors is what a caller acts on: true on an error, false on warnings
// alone and on a clean document.
func TestHasErrorsTellsErrorsFromWarnings(t *testing.T) {
	clean, err := os.ReadFile(filepath.Join(fixtureDir("valid"), "minimal-v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if res := Parse("clean.yaml", clean); res.HasErrors() {
		t.Errorf("a clean document has errors: %v", res.Diagnostics)
	}
	warned, err := os.ReadFile(filepath.Join(fixtureDir("valid"), "prompt-body-settled.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if res := Parse("warned.yaml", warned); res.HasErrors() || len(res.Diagnostics) == 0 {
		t.Errorf("a warned document has errors, or no diagnostic: %v", res.Diagnostics)
	}
	if res := Parse("bad.yaml", []byte("dsl: 2\nnodes:\n  - agent: a\n    max_tokens: -1\n")); !res.HasErrors() {
		t.Errorf("a refused document has no error: %v", res.Diagnostics)
	}
}
