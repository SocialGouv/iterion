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

// CommonMark pairs a backtick run with the next run of EXACTLY its length; a
// run with no partner is literal text. A scanner that pairs runs of any two
// lengths masks the prose between two literal runs — and a real link written
// there leaves every artifact built from the mask.
func TestSpansPairRunsOfTheSameLengthOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		src     string
		visible []string
		hidden  []string
	}{
		{
			name:    "a real link between two literal runs of different lengths",
			src:     "prose ` and a real link [b](b.md) and then ``",
			visible: []string{"[b](b.md)"},
		},
		{
			name:    "a double run closes on a double run, not on the single one after it",
			src:     "``a `b` c`` then [d](d.md)",
			hidden:  []string{"a `b` c"},
			visible: []string{"[d](d.md)"},
		},
		{
			name:    "a single run closes on the next single run",
			src:     "`](x.md)` and [y](y.md)",
			hidden:  []string{"](x.md)"},
			visible: []string{"[y](y.md)"},
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
					t.Errorf("the mask swallowed the prose %q:\n%s", v, masked)
				}
			}
		})
	}
}

// A backtick fence's info string may not contain a backtick, so "```go `x`"
// is a paragraph. Read as an opener it is never closed, and every link to the
// end of the page disappears from whatever the mask feeds.
func TestAnInfoStringCarryingABacktickDoesNotOpenAFence(t *testing.T) {
	const src = "# A\n\n```go `x`\n\nA real link to [b](b.md).\n"
	masked := mdcode.Mask(src)
	if !strings.Contains(masked, "[b](b.md)") {
		t.Errorf("a line that only LOOKS like a fence opener blanked the rest of the page:\n%s", masked)
	}
	// A genuine opener still opens: the guard must not disarm fences.
	const fenced = "# A\n\n```go\n[b](b.md)\n```\n"
	if strings.Contains(mdcode.Mask(fenced), "[b](b.md)") {
		t.Error("a real fenced block is no longer masked")
	}
}

// A caller that truncates prose can cut between a code span's two runs. The
// half span renders a stray backtick, and every scanner downstream reads what
// the span was quoting as prose.
func TestCutBeforeDanglingSpanRefusesToEndInsideASpan(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"a cut inside a span drops the half span", "the form `](../../docs/foo.md) ou", "the form "},
		{"a closed span is kept whole", "the form `](../x.md)` and more", "the form `](../x.md)` and more"},
		{"prose with no backtick is untouched", "just prose", "just prose"},
		{"a closed span followed by a dangling run", "`a` then `b", "`a` then "},
		{"a dangling double run", "text ``half", "text "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mdcode.CutBeforeDanglingSpan(tc.src); got != tc.want {
				t.Errorf("CutBeforeDanglingSpan(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}
