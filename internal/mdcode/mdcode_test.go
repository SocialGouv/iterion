package mdcode_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/mdcode"
)

// Mask's contract is an OFFSET contract: every caller runs its own pattern
// over the mask and reads the bytes out of the original, so a mask one byte
// shorter than its input silently shifts every rewrite that follows it.
func TestMaskKeepsEveryOffset(t *testing.T) {
	for _, src := range []string{
		"", "plain", "`code`", "a `b` c", "``a `b` c``",
		"# A heading with `code`\n\ntext\n```go\nfenced()\n```\ntail\n",
		"an em-dash — and an accented é inside `a spàn` and out",
		"unclosed ` backtick to the end",
		"~~~\ntilde fence\n~~~\n",
	} {
		masked := mdcode.Mask(src)
		if len(masked) != len(src) {
			t.Errorf("Mask(%q) is %d bytes for a %d-byte input", src, len(masked), len(src))
		}
		if strings.Count(masked, "\n") != strings.Count(src, "\n") {
			t.Errorf("Mask(%q) has %d newlines, the input has %d", src, strings.Count(masked, "\n"), strings.Count(src, "\n"))
		}
	}
}

// Each row is one form the corpus writes. `want` is what a link scanner must
// still SEE after masking: the code forms must vanish, the prose forms must
// survive byte for byte at their own offsets.
func TestMaskHidesCodeAndKeepsProse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		src     string
		visible []string
		hidden  []string
	}{
		{
			name:    "a single-backtick span",
			src:     "The map used to emit `](../../docs/foo.md)`, which github.com renders.",
			hidden:  []string{"](../../docs/foo.md)"},
			visible: []string{"The map used to emit", "which github.com renders."},
		},
		{
			name:    "a double-backtick span, which is how a span carrying a backtick is written",
			src:     "Written ``](`x`.md)`` in the page.",
			hidden:  []string{"](`x`.md)"},
			visible: []string{"Written", "in the page."},
		},
		{
			name:    "a real link next to a quoted one keeps its own offsets",
			src:     "See [the page](../dsl.md), never `](../../docs/dsl.md)`.",
			hidden:  []string{"](../../docs/dsl.md)"},
			visible: []string{"](../dsl.md)"},
		},
		{
			name:    "a fenced block, delimiters included",
			src:     "before\n```\n[x](../../y.md)\n```\nafter [z](w.md)\n",
			hidden:  []string{"[x](../../y.md)", "```"},
			visible: []string{"before", "after [z](w.md)"},
		},
		{
			name: "a stray backtick opens nothing, exactly as a renderer reads it",
			src:  "A link [x](y.md) after one ` backtick.",
			// Nothing is hidden: an unmatched run is literal text, so the
			// link stays a link. Masking to end-of-string here would hide a
			// real link from every scanner at once.
			visible: []string{"[x](y.md)"},
		},
		{
			name:    "a fence inside a blockquote",
			src:     "> ```\n> [q](../../q.md)\n> ```\nout [r](r.md)\n",
			hidden:  []string{"[q](../../q.md)"},
			visible: []string{"out [r](r.md)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			masked := mdcode.Mask(tc.src)
			for _, h := range tc.hidden {
				if strings.Contains(masked, h) {
					t.Errorf("the mask still carries the code %q:\n%s", h, masked)
				}
			}
			for _, v := range tc.visible {
				if !strings.Contains(masked, v) {
					t.Errorf("the mask lost the prose %q:\n%s", v, masked)
				}
				if strings.Index(masked, v) != strings.Index(tc.src, v) {
					t.Errorf("the prose %q moved from offset %d to %d", v, strings.Index(tc.src, v), strings.Index(masked, v))
				}
			}
		})
	}
}

// The two patterns are this package's whole definition of code, and the
// scanners that strip rather than mask read them from here. A pattern that
// stopped recognising a form would let that form through every caller.
func TestThePatternsRecogniseTheFormsTheCorpusWrites(t *testing.T) {
	for _, span := range []string{"`x`", "``x``", "```x```"} {
		if m := mdcode.SpanPattern().FindString(span); m != span {
			t.Errorf("SpanPattern reads %q out of %q", m, span)
		}
	}
	if mdcode.SpanPattern().MatchString("no code here") {
		t.Error("SpanPattern matches prose")
	}
	for _, fence := range []string{"```", "```go", "~~~", "> ```", "   ````"} {
		if !mdcode.FencePattern().MatchString(fence) {
			t.Errorf("FencePattern does not recognise %q", fence)
		}
	}
	if mdcode.FencePattern().MatchString("`` not a fence") {
		t.Error("FencePattern treats a two-backtick run as a fence")
	}
}
