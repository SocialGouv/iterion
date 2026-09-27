package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A word-valued property (an enum word, an ident, a type, a backend or a
// model id) cannot carry a block scalar's final line break: the reader used
// to spell it `"value\n"` and let the run refuse what compile should have
// (#1781). It is refused at the key's line, where the `|` sits.
func TestABlockScalarUnderAWordValuedPropertyIsRefusedAtTheKeysLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prop  string
		value string
		line  int
	}{
		{"a backend written as a literal block", "backend: |", "claude_code", 5},
		{"a backend written as a folded block", "backend: >", "claude_code", 5},
		{"an interaction mode written as a block", "interaction: |", "none", 5},
		{"a provider written as a block", "provider: |", "anthropic", 5},
		{"an interaction model written as a block", "interaction_model: |", "anthropic/claude-opus-5", 5},
		{"a model written as a block", "model: |", "anthropic/claude-opus-5", 4},
		{"a timeout written as a block", "timeout: |", "20m", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			propLine := strings.Repeat(" ", 4) + tc.prop
			valueLine := "      " + tc.value
			lines := []string{
				"dsl: 2",
				"nodes:",
				"  - agent: a",
				propLine,
				valueLine,
				"workflow:",
				"  name: w",
				"  entry: a",
				"  edges:",
				"    - a -> done",
			}
			if tc.line == 5 {
				lines = []string{
					"dsl: 2",
					"nodes:",
					"  - agent: a",
					"    model: m",
					propLine,
					valueLine,
					"workflow:",
					"  name: w",
					"  entry: a",
					"  edges:",
					"    - a -> done",
				}
			}
			src := strings.Join(lines, string(rune(10))) + string(rune(10))
			res := Parse("x.yaml", []byte(src))
			if !res.HasErrors() {
				t.Fatalf("the block scalar under %q is accepted:%s%s", tc.prop, string(rune(10)), src)
			}
			found := false
			for _, d := range res.Diagnostics {
				if strings.Contains(d.Message, "takes a word on the key's line, not a `|` or `>` block") {
					found = true
					if d.Line != tc.line {
						t.Errorf("the refusal sits on line %d, want the key's line %d", d.Line, tc.line)
					}
				}
			}
			if !found {
				t.Fatalf("no refusal names the block:%s%v", string(rune(10)), res.Diagnostics)
			}
		})
	}
}

// A word on the key's line still reads — bare, quoted, or as a template —
// and a text property keeps its block.
func TestAWordOnTheKeysLineAndABlockOfTextStillRead(t *testing.T) {
	nl := string(rune(10))
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"a bare word", "claude_code"},
		{"a quoted word", `"claude_code"`},
		{"a template", `"{{vars.backend}}"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Join([]string{
				"dsl: 2",
				"nodes:",
				"  - agent: a",
				"    model: m",
				"    backend: " + tc.value,
				"workflow:",
				"  name: w",
				"  entry: a",
				"  edges:",
				"    - a -> done",
			}, nl) + nl
			res := Parse("x.yaml", []byte(src))
			if res.HasErrors() {
				t.Fatalf("the document is refused: %v%s%s", res.Diagnostics, nl, src)
			}
			if got := res.File.Agents[0].Backend; got != "claude_code" && got != `{{vars.backend}}` {
				t.Errorf("the backend came back as %q", got)
			}
		})
	}
	t.Run("a command keeps its block", func(t *testing.T) {
		src := strings.Join([]string{
			"dsl: 2",
			"nodes:",
			"  - tool: t",
			"    command: |",
			"      echo hi",
			"      echo bye",
			"workflow:",
			"  name: w",
			"  entry: t",
			"  edges:",
			"    - t -> done",
		}, nl) + nl
		res := Parse("x.yaml", []byte(src))
		if res.HasErrors() {
			t.Fatalf("the document is refused: %v%s%s", res.Diagnostics, nl, src)
		}
		if got := res.File.Tools[0].Command; got != "echo hi\necho bye\n" {
			t.Errorf("the command came back as %q, want both lines", got)
		}
	})
}

// The parser echoes the value it refuses through %q: a control character
// shows as its escape and never breaks the message's line. Probed on the
// .bot text directly — the converter refuses a quoted value with a line
// break under a word form before the parser sees it (the gate above).
func TestTheParserQuotesTheValueItEchoes(t *testing.T) {
	nl := string(rune(10))
	src := strings.Join([]string{
		"dsl: 2",
		"agent a:",
		"  model: m",
		`  interaction: "none\n"`,
		"workflow w:",
		"  entry: a",
		"  a -> done",
	}, nl) + nl
	res := parser.Parse("x.bot", src)
	found := false
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, "interaction mode") {
			found = true
			if strings.ContainsRune(d.Message, rune(10)) {
				t.Errorf("the message breaks its own line:%q", d.Message)
			}
			if !strings.Contains(d.Message, `got "none\n"`) {
				t.Errorf("the value is not quoted as written: %q", d.Message)
			}
		}
	}
	if !found {
		t.Fatalf("no diagnostic names the mode:%s%v", nl, res.Diagnostics)
	}
}

// A quoted value carrying a line break is as broken as a clip block: the
// reading is what a word cannot hold, not a spelling.
func TestAQuotedValueCarryingALineBreakIsRefused(t *testing.T) {
	nl := string(rune(10))
	src := strings.Join([]string{
		"dsl: 2",
		"nodes:",
		"  - agent: a",
		`    backend: "claw\n"`,
		"    model: m",
		"workflow:",
		"  name: w",
		"  entry: a",
		"  edges:",
		"    - a -> done",
	}, nl) + nl
	res := Parse("x.yaml", []byte(src))
	if !res.HasErrors() {
		t.Fatalf("a quoted backend with a line break is accepted:%s%s", nl, src)
	}
	found := false
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, "carries a line break") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal does not name the break:%s%v", nl, res.Diagnostics)
	}
}

// A strip chomping (`|-`/`>-`) takes the scalar's break back off: a
// one-line chomped block reads exactly the word and stays a word's
// spelling — only a block whose READING keeps a break is refused.
func TestAChompedSingleLineBlockStillReadsAsTheWord(t *testing.T) {
	nl := string(rune(10))
	for _, indicator := range []string{"|-", ">-"} {
		t.Run(indicator, func(t *testing.T) {
			src := strings.Join([]string{
				"dsl: 2",
				"nodes:",
				"  - agent: a",
				"    model: m",
				"    backend: " + indicator,
				"      claude_code",
				"workflow:",
				"  name: w",
				"  entry: a",
				"  edges:",
				"    - a -> done",
			}, nl) + nl
			res := Parse("x.yaml", []byte(src))
			if res.HasErrors() {
				t.Fatalf("a chomped one-line block reads the word and must not be refused: %v%s%s", res.Diagnostics, nl, src)
			}
			if got := res.File.Agents[0].Backend; got != "claude_code" {
				t.Errorf("the backend came back as %q, want claude_code", got)
			}
		})
	}
}

// The workflow's default_backend is the twin of a node's backend: the same
// word gate covers it (the gate's round-1 finding on #1881).
func TestADefaultBackendBlockIsRefused(t *testing.T) {
	nl := string(rune(10))
	src := strings.Join([]string{
		"dsl: 2",
		"workflow:",
		"  name: w",
		"  default_backend: |",
		"    claw",
		"  entry: done",
	}, nl) + nl
	res := Parse("x.yaml", []byte(src))
	if !res.HasErrors() {
		t.Fatalf("the block under default_backend is accepted:%s%s", nl, src)
	}
	found := false
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, "takes a word on the key's line, not a `|` or `>` block") {
			found = true
			if d.Line != 4 {
				t.Errorf("the refusal sits on line %d, want the key's line 4", d.Line)
			}
		}
	}
	if !found {
		t.Fatalf("no refusal names the block:%s%v", nl, res.Diagnostics)
	}
}
