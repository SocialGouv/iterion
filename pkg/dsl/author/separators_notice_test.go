package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The document-level notice of #1663: a scan of the RAW source, before any
// reading of it, names every line holding a U+0085, U+2028 or U+2029 —
// because a trailing line made only of such a character is a trailing blank
// line to the scanner, chomped before the converter's own reading (E053)
// can see it.
func TestInvisibleLineSeparatorsGetOneDocumentNotice(t *testing.T) {
	nl, ls, ps, nel := string(rune(10)), string(rune(0x2028)), string(rune(0x2029)), string(rune(0x85))
	tail := nl + "nodes:" + nl + "  - agent: a" + nl + "    model: m" + nl + "    system: p" + nl

	// A `|` body `one` followed by a blank line and a line holding only
	// U+2028: the scanner chomps that trailing line before the body is
	// read, so the body's own warning never sees it — the notice does.
	t.Run("a trailing line holding only a separator", func(t *testing.T) {
		doc := "dsl: 2" + nl + "prompts:" + nl + "  p: |" + nl + "    one" + nl + nl + ls + tail
		res := Parse("p.yaml", []byte(doc))
		if res.HasErrors() {
			t.Fatalf("the document is refused: %v", res.Diagnostics)
		}
		if got := res.File.Prompts[0].Body; got != "one" {
			t.Fatalf("the body reads %q, want the visible text %q", got, "one")
		}
		d := noticeOf(t, res.Diagnostics)
		if d.Line != 6 || !strings.Contains(d.Message, "line 6") {
			t.Errorf("the notice does not name line 6: line %d, %q", d.Line, d.Message)
		}
		if n := countCode(res.Diagnostics, parser.DiagAuthorSeparators); n != 1 {
			t.Errorf("%d separator notices, want exactly one per document", n)
		}
		// And the body's own reading said nothing: chomping hid it.
		if n := countCode(res.Diagnostics, parser.DiagAuthorPromptBody); n != 0 {
			t.Errorf("the chomped separator reached a body warning: %v", res.Diagnostics)
		}
	})

	// With keep chomping (`|+`) the separator is preserved INTO the value:
	// the notice, plus the existing body warnings that read it.
	t.Run("a kept separator is noticed twice", func(t *testing.T) {
		doc := "dsl: 2" + nl + "prompts:" + nl + "  p: |+" + nl + "    one" + nl + nl + ls + tail
		res := Parse("p.yaml", []byte(doc))
		if res.HasErrors() {
			t.Fatalf("the document is refused: %v", res.Diagnostics)
		}
		noticeOf(t, res.Diagnostics)
		if n := countCode(res.Diagnostics, parser.DiagAuthorPromptBody); n == 0 {
			t.Errorf("the kept separator lost its body warning: %v", res.Diagnostics)
		}
	})

	// Every occurrence is listed, whichever of the three it is, at the
	// physical line the scanner counts.
	t.Run("every separator of the document is listed", func(t *testing.T) {
		doc := "dsl: 2" + nl + "## " + nel + nl + "prompts:" + nl + "  p: |" + nl + "    one" + ls + "    two" + nl + ps + tail
		res := Parse("p.yaml", []byte(doc))
		d := noticeOf(t, res.Diagnostics)
		for _, want := range []string{"3 invisible line separators", "lines 2, 6, 8"} {
			if !strings.Contains(d.Message, want) {
				t.Errorf("the notice does not say %q: %q", want, d.Message)
			}
		}
	})

	// A document with none is not warned, and an escaped `\L` in a quoted
	// string is the author's character, not a raw separator.
	t.Run("a document with none gets no notice", func(t *testing.T) {
		doc := "dsl: 2" + nl + "prompts:" + nl + `  p: "one\Ltwo"` + tail
		res := Parse("p.yaml", []byte(doc))
		if res.HasErrors() {
			t.Fatalf("the document is refused: %v", res.Diagnostics)
		}
		if n := countCode(res.Diagnostics, parser.DiagAuthorSeparators); n != 0 {
			t.Errorf("a document with no raw separator is warned: %v", res.Diagnostics)
		}
	})
}

// noticeOf is the document's one E055, or a failure.
func noticeOf(t *testing.T, diags []parser.Diagnostic) parser.Diagnostic {
	t.Helper()
	for _, d := range diags {
		if d.Code == parser.DiagAuthorSeparators {
			if d.Severity != parser.SeverityWarning {
				t.Fatalf("the separator notice is a %s: %s", d.Severity, d.Message)
			}
			return d
		}
	}
	t.Fatalf("no E055 notice among %v", diags)
	return parser.Diagnostic{}
}

func countCode(diags []parser.Diagnostic, code parser.DiagCode) int {
	n := 0
	for _, d := range diags {
		if d.Code == code {
			n++
		}
	}
	return n
}
