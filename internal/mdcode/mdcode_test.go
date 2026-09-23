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
func TestCloseDanglingSpanKeepsTheTextAndClosesTheSpan(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"a span the cut opened is closed, not dropped", "the form `](../../docs/foo.md) ou", "the form `](../../docs/foo.md) ou`"},
		{"a closed span is kept whole", "the form `](../x.md)` and more", "the form `](../x.md)` and more"},
		{"prose with no backtick is untouched", "just prose", "just prose"},
		{"a closed span followed by a dangling run", "`a` then `b", "`a` then `b`"},
		{"a dangling double run", "text ``half", "text ``half``"},
		{"a run with nothing after it opened nothing", "text `", "text "},
		{"the dangling run sits BEFORE a later closed span", "the form `](../x.md) then ``double`` quoted", "the form `](../x.md) then ``double`` quoted`"},
		{"the closing run must not merge with a trailing run", "``a`", "``a` ``"},
		{"a wider trailing run does not close a narrower opener", "a `b``", "a `b`` `"},
		{"a run followed only by space is dropped too", "text `  ", "text "},
		// The cell whose payload the first version of this helper threw away.
		{"the real corpus cell keeps its name", "the stdio server (`iterion", "the stdio server (`iterion`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mdcode.CloseDanglingSpan(tc.src)
			if got != tc.want {
				t.Errorf("CloseDanglingSpan(%q) = %q, want %q", tc.src, got, tc.want)
			}
			// Whatever it returns must read as code-complete — asserted
			// against Spans, not against CloseDanglingSpan itself: a witness
			// that asks the function under test whether it succeeded passes
			// on exactly the inputs the function is blind to.
			if pos, ok := danglingBacktick(got); ok {
				t.Errorf("CloseDanglingSpan(%q) = %q, still dangling at byte %d", tc.src, got, pos)
			}
			if again := mdcode.CloseDanglingSpan(got); again != got {
				t.Errorf("not idempotent: %q then %q", got, again)
			}
		})
	}
}

// A truncation must never empty a cell: firstSentence cuts the ADR status
// column at 60 bytes, where a code span crosses the bound easily, and a cut
// at the FIRST backtick used to leave the whole cell empty.
func TestCloseDanglingSpanNeverEmptiesATextThatHadText(t *testing.T) {
	for _, src := range []string{
		"`Remplacé par ADR-140 le 2026-04-12, voir la note de",
		"`iterion",
		"``a",
		"a `b",
	} {
		if got := mdcode.CloseDanglingSpan(src); strings.TrimSpace(strings.Trim(got, "`")) == "" {
			t.Errorf("CloseDanglingSpan(%q) = %q — a cell that had text lost all of it", src, got)
		}
	}
}

// danglingBacktick reports a backtick byte that no code span covers — the
// independent oracle for "this string is code-complete".
func danglingBacktick(s string) (int, bool) {
	spans := mdcode.Spans(s)
	for i := 0; i < len(s); i++ {
		if s[i] != '`' {
			continue
		}
		inside := false
		for _, r := range spans {
			if i >= r[0] && i < r[1] {
				inside = true
				break
			}
		}
		if !inside {
			return i, true
		}
	}
	return 0, false
}

// Every string this repository can truncate must come out code-complete. An
// exhaustive sweep is what found the merge bug: the closing run appended to a
// text already ending in backticks widened the trailing run instead.
func TestCloseDanglingSpanLeavesNoDanglingBacktick(t *testing.T) {
	alphabet := []string{"`", "a", " "}
	var build func(prefix string, depth int)
	checked := 0
	build = func(prefix string, depth int) {
		if depth == 0 {
			got := mdcode.CloseDanglingSpan(prefix)
			checked++
			if pos, ok := danglingBacktick(got); ok {
				t.Fatalf("CloseDanglingSpan(%q) = %q, dangling at %d", prefix, got, pos)
			}
			if again := mdcode.CloseDanglingSpan(got); again != got {
				t.Fatalf("CloseDanglingSpan(%q) = %q is not idempotent: %q", prefix, got, again)
			}
			return
		}
		for _, c := range alphabet {
			build(prefix+c, depth-1)
		}
	}
	for n := 0; n <= 7; n++ {
		build("", n)
	}
	if checked < 3000 {
		t.Fatalf("only %d strings swept — this test would prove little", checked)
	}
}
