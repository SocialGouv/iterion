package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A ` #` after a plain text value starts a comment: the value ends there
// and the rest of the line is dropped — validate used to read it green and
// the loss surfaced only at the run. It is said where the author looks
// first, as a warning at the value's line (#1780).
func TestACommentEndingAPlainValueWarns(t *testing.T) {
	nl := string(rune(10))
	src := strings.Join([]string{
		"dsl: 2",
		"nodes:",
		"  - tool: t",
		`    command: echo "see #123"`,
		"workflow:",
		"  name: w",
		"  entry: t",
		"  edges:",
		"    - t -> done",
	}, nl) + nl
	res := Parse("x.yaml", []byte(src))
	if res.HasErrors() {
		t.Fatalf("a warning must not refuse the document: %v", res.Diagnostics)
	}
	var warned bool
	for _, d := range res.Diagnostics {
		if d.Code == parser.DiagAuthorPromptBody && d.Severity == parser.SeverityWarning {
			warned = true
			if d.Line != 4 {
				t.Errorf("the warning sits on line %d, want the value's line 4", d.Line)
			}
			if !strings.Contains(d.Message, `echo \"see`) {
				t.Errorf("the warning does not name the value as read: %q", d.Message)
			}
		}
	}
	if !warned {
		t.Fatalf("no warning says the comment cut the value:%s%v", nl, res.Diagnostics)
	}
	if got := res.File.Tools[0].Command; got != `echo "see` {
		t.Fatalf("the command came back as %q, want the cut reading the warning names", got)
	}
}

// A comment may end a plain value wherever text is taken.
func TestACommentEndingAnyPlainTextFieldWarns(t *testing.T) {
	nl := string(rune(10))
	src := strings.Join([]string{
		"dsl: 2",
		"nodes:",
		"  - agent: a",
		"    model: m",
		"    description: Fix issue #123",
		"workflow:",
		"  name: w",
		"  entry: a",
		"  edges:",
		"    - a -> done",
	}, nl) + nl
	res := Parse("x.yaml", []byte(src))
	if res.HasErrors() {
		t.Fatalf("a warning must not refuse the document: %v", res.Diagnostics)
	}
	for _, d := range res.Diagnostics {
		if d.Code == parser.DiagAuthorPromptBody && d.Severity == parser.SeverityWarning {
			return
		}
	}
	t.Fatalf("no warning says the comment cut the description:%s%v", nl, res.Diagnostics)
}

// Every other placement of a ` #` is plainly a comment: a number, a bool or
// a null keeps its note, a quoted or block scalar carries its text, and a
// comment on a line of its own has no value to cut.
func TestACommentThatCannotBeACutValueStaysSilent(t *testing.T) {
	nl := string(rune(10))
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"a number keeps its note", []string{
			"dsl: 2",
			"nodes:",
			"  - agent: a",
			"    model: m",
			"    max_tokens: 4096  # keep it low",
			"workflow:",
			"  name: w",
			"  entry: a",
			"  edges:",
			"    - a -> done",
		}},
		{"a bool keeps its note", []string{
			"dsl: 2",
			"nodes:",
			"  - agent: a",
			"    model: m",
			"    readonly: true  # read-only",
			"workflow:",
			"  name: w",
			"  entry: a",
			"  edges:",
			"    - a -> done",
		}},
		{"a key keeps its note", []string{
			"dsl: 2",
			"workflow:  # the run",
			"  name: w",
			"  entry: done",
		}},
		{"a quoted value carries the note outside", []string{
			"dsl: 2",
			"nodes:",
			"  - agent: a",
			`    backend: "claw"  # the engine`,
			"    model: m",
			"workflow:",
			"  name: w",
			"  entry: a",
			"  edges:",
			"    - a -> done",
		}},
		{"a block scalar holds its sharp", []string{
			"dsl: 2",
			"nodes:",
			"  - tool: t",
			"    command: |",
			"      echo see #123",
			"workflow:",
			"  name: w",
			"  entry: t",
			"  edges:",
			"    - t -> done",
		}},
		{"a comment on a line of its own", []string{
			"dsl: 2",
			"nodes:",
			"  - tool: t",
			"    # a note of its own",
			`    command: echo hi`,
			"workflow:",
			"  name: w",
			"  entry: t",
			"  edges:",
			"    - t -> done",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Join(tc.lines, nl) + nl
			res := Parse("x.yaml", []byte(src))
			if res.HasErrors() {
				t.Fatalf("the document is refused: %v%s%s", res.Diagnostics, nl, src)
			}
			for _, d := range res.Diagnostics {
				if d.Code == parser.DiagAuthorPromptBody && d.Severity == parser.SeverityWarning {
					t.Errorf("a comment that cannot be a cut value warns: %q", d.Message)
				}
			}
		})
	}
}
