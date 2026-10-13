package mdcode_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

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

// The scanner's patterns are asymmetric, and that asymmetry IS the rule: a
// list marker starts a new list item, so it can introduce an OPENER and never
// a closer. This package owns the rule, so this package has to be able to
// redden on it — the regression that taught it was invisible to
// `go test ./internal/mdcode/...`.
func TestOnlyAnOpenerMayCarryAListMarker(t *testing.T) {
	for _, tc := range []struct{ name, opener, inner, closer string }{
		{"a bullet", "- ```", "+ ```", "  ```"},
		{"a numbered item", "1. ```", "- ```", "   ```"},
		{"a blockquote", "> ```sh", "> - ```", "> ```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f mdcode.FenceScanner
			if !f.Code(tc.opener) || !f.Open() {
				t.Fatalf("%q did not open a fence", tc.opener)
			}
			if !f.Code(tc.inner) {
				t.Errorf("%q inside the block is not code", tc.inner)
			}
			if !f.Open() {
				t.Errorf("%q closed the block — a list marker introduces an item, never a closer", tc.inner)
			}
			if !f.Code(tc.closer) || f.Open() {
				t.Errorf("%q did not close the block opened by %q", tc.closer, tc.opener)
			}
		})
	}
}

// Every other witness of this package measures markdown with this package,
// so the predicate under test certifies itself: a byte it misreads is a byte
// none of them can see. This one holds the answer of the renderer that serves
// the committed .md — GitHub's /markdown API — captured on every case below
// (2026-10-12, the nine list-item cases 2026-10-13) and written down as the
// expectation. The reader a future change must satisfy is GitHub's, not
// ours.
//
//	in   "```\nfoo\n\t```\nAFTER THE BLOCK: assignee rules\n"
//	out  <pre><code>foo\n\t```\nAFTER THE BLOCK: assignee rules\n</code></pre>
//	in   "prose before\r```suggestion\rswallowed one\rswallowed two\r"
//	out  <p>prose before</p><pre lang="suggestion"><code>swallowed one\nswallowed two\n</code></pre>
//
// pandoc 3.1.3 (commonmark and gfm alike) was asked the same questions and
// agrees on indentation, on CRLF and on every container case below — but it
// DELETES a lone carriage return instead of breaking the line on it, so on
// the two cases that carry one it answers about a different document and
// witnesses nothing. Which is why the expectation is named, not averaged.
func TestFenceReadingMatchesTheRenderersOnIndentationAndLineEndings(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		open bool
		why  string
	}{
		{
			name: "a closer three columns in closes",
			src:  "```\nfoo\n   ```\nAFTER\n",
			open: false,
			why:  "<pre><code>foo</code></pre><p>AFTER</p>",
		},
		{
			name: "a closer four columns in is block content",
			src:  "```\nfoo\n    ```\nAFTER\n",
			open: true,
			why:  "the closer and AFTER are inside the block",
		},
		{
			// A tab is four columns wherever indentation defines block
			// structure, so a tab-indented delimiter closes nothing. Counted
			// in bytes it measured one column and closed the block, which is
			// how a hand-written manifest example re-opened the phantom
			// fence with no escape sequence at all.
			name: "a tab-indented closer is block content",
			src:  "```\nfoo\n\t```\nAFTER THE BLOCK: assignee rules\n",
			open: true,
			why:  `<pre><code>foo\n\t` + "```" + `\nAFTER THE BLOCK: assignee rules</code></pre>`,
		},
		{
			// Measured, not deduced: inside a blockquote the same tab lands
			// two columns past the quote's content column, which is within
			// the three a closer may take, and both renderers close the
			// block. A rule counting the tab as one column would close it
			// too — and would keep closing the column-0 case above, which is
			// the one that corrupts the catalogue. So this case is what
			// stops the column count from being written as "a tab never
			// closes".
			name: "a tab-indented closer inside a blockquote still closes",
			src:  "> ```\n> foo\n> \t```\n> AFTER\n",
			open: false,
			why:  "<blockquote><pre><code>foo</code></pre><p>AFTER</p></blockquote>",
		},
		{
			name: "a lone carriage return is a line ending, so the run after it opens",
			src:  "prose before\r```suggestion\rswallowed one\rswallowed two\r",
			open: true,
			why:  `<p>prose before</p><pre lang="suggestion">…`,
		},
		{
			name: "a carriage return and line feed is one line ending",
			src:  "prose before\r\n```suggestion\r\nswallowed\r\n",
			open: true,
			why:  "the same block, opened after the CRLF",
		},
		{
			name: "a fence opened and closed across carriage returns is balanced",
			src:  "prose\r```yaml\rnodes: []\r```\rand on\r",
			open: false,
			why:  `<p>prose</p><div class="highlight highlight-source-yaml">…</div><p>and on</p>`,
		},
		{
			// The ticket's own shape: a description word-wrapped onto a
			// ```suggestion line INSIDE a list item. The item ends at the
			// column-0 line below, which closes the block the item held, so
			// the delimiter after it is a NEW opener and takes the rest of
			// the page — a reading that pairs the two delimiters calls this
			// balanced and escapes nothing.
			name: "a list item's fence is closed by the end of the item",
			src:  "What it posts:\n- ```suggestion\nthe replacement line\n```\nAFTER: assignee rules\n",
			open: true,
			why:  `<ul><li><pre lang="suggestion"><code></code></pre></li></ul><p>the replacement line</p><pre><code>AFTER: assignee rules</code></pre>`,
		},
		{
			name: "an ordered item's fence is closed by the end of the item",
			src:  "What it posts:\n1. ```suggestion\nthe replacement line\n```\nAFTER: assignee rules\n",
			open: true,
			why:  "the same, with <ol> for <ul>",
		},
		{
			name: "a blockquote's fence is closed by the end of the quote",
			src:  "Note:\n> ```sh\n> run it\n```\nAFTER: assignee rules\n",
			open: true,
			why:  `<blockquote><div class="highlight highlight-source-shell"><pre>run it</pre></div></blockquote><pre><code>AFTER: assignee rules</code></pre>`,
		},
		{
			// The spelling an author actually writes, and the one no row
			// above asks about: the container is NOT on the delimiter's own
			// line. The example is indented under the bullet that introduces
			// it, and the closing delimiter is back at the bullet's column —
			// so the item ends there, the block ends with it, and that
			// delimiter OPENS one. A rule reading the container off the
			// delimiter's line sees nothing to hold the opener, pairs the
			// two delimiters, and calls the field balanced.
			name: "a fence indented into an item opened ABOVE is closed by the end of the item",
			src:  "What it posts:\n- on a PR it posts:\n  ```suggestion\n  the replacement line\n```\nAFTER: assignee rules\n",
			open: true,
			why:  `<ul><li>on a PR it posts:<pre lang="suggestion"><code>the replacement line</code></pre></li></ul><pre><code>AFTER: assignee rules</code></pre>`,
		},
		{
			// Its negative control, one column apart: the closer INSIDE the
			// item closes the block there and leaves nothing open. A rule
			// ending the item on any delimiter below the opener would call
			// this open and spend a backslash on a sound example.
			name: "a fence an item opened above closes inside itself is balanced",
			src:  "What it posts:\n- on a PR it posts:\n  ```suggestion\n  the replacement line\n  ```\nAFTER: assignee rules\n",
			open: false,
			why:  `<ul><li>on a PR it posts:<pre lang="suggestion"><code>the replacement line</code></pre></li></ul><p>AFTER: assignee rules</p>`,
		},
		{
			// An item that ENDED holds nothing: the fence below it is the
			// page's own, three columns in at most, and the column-0
			// delimiter closes it. A reading that kept the item alive would
			// end the block at that delimiter and read it as a new opener —
			// a phantom fence of its own making.
			name: "an item that ended holds no fence opened after it",
			src:  "- an item\n\nprose\n  ```a\n  x\n```\nAFTER: assignee rules\n",
			open: false,
			why:  `<ul><li>an item</li></ul><p>prose</p><pre lang="a"><code>x</code></pre><p>AFTER: assignee rules</p>`,
		},
		{
			// A blank line ends no item, so the fence under it IS held by
			// the item and the column-0 delimiter ends both.
			name: "a blank line between an item and its fence ends neither",
			src:  "- an item:\n\n  ```a\n  x\n```\nAFTER: assignee rules\n",
			open: true,
			why:  `<ul><li><p>an item:</p><pre lang="a"><code>x</code></pre></li></ul><pre><code>AFTER: assignee rules</code></pre>`,
		},
		{
			// A thematic break carries a marker and a space and opens no
			// item: a reading that took it for one would hold the fence
			// below at a column no container has.
			name: "a thematic break is no list item",
			src:  "* * *\n  ```a\n  x\n```\nAFTER: assignee rules\n",
			open: false,
			why:  `<hr><pre lang="a"><code>x</code></pre><p>AFTER: assignee rules</p>`,
		},
		{
			// A marker takes a space or the end of its line; `-x` is a word.
			name: "a marker byte glued to a word is no list item",
			src:  "-x is not a marker\n  ```a\n  x\n```\nAFTER: assignee rules\n",
			open: false,
			why:  `<p>-x is not a marker</p><pre lang="a"><code>x</code></pre><p>AFTER: assignee rules</p>`,
		},
		{
			// Under a prose line, an ordered marker starting elsewhere than
			// 1 opens NO list: both renderers read it as paragraph text, so
			// the fence below it is the page's own and the column-0
			// delimiter closes it. A rule holding that fence inside an item
			// it imagined would escape this CLOSER and leave the opener
			// live — the generator writing the phantom fence itself.
			name: "an ordered marker that cannot interrupt a paragraph opens no item",
			src:  "prose the bundle wrote\n2) a step\n   ```a\n   x\n```\nAFTER: assignee rules\n",
			open: false,
			why:  `<p>prose the bundle wrote<br>2) a step</p><pre lang="a"><code>x</code></pre><p>AFTER: assignee rules</p>`,
		},
		{
			// Its positive twin, one blank line apart: no paragraph is open
			// above, so the SAME marker opens an item, the fence is inside
			// it, and the column-0 delimiter ends the item and opens a block
			// over everything after it.
			name: "after a blank line the same marker opens an item that holds the fence",
			src:  "prose the bundle wrote\n\n2) a step\n   ```a\n   x\n```\nAFTER: assignee rules\n",
			open: true,
			why:  `<ol start="2"><li>a step<pre lang="a"><code>x</code></pre></li></ol><pre><code>AFTER: assignee rules</code></pre>`,
		},
		{
			// A lone marker under a prose line is no item either — it is a
			// setext heading underline, and both renderers show the prose
			// above it as a heading.
			name: "a lone marker under a prose line is a setext heading, not an item",
			src:  "prose the bundle wrote\n-\n  ```a\n  x\n```\nAFTER: assignee rules\n",
			open: false,
			why:  `<h2>prose the bundle wrote</h2><pre lang="a"><code>x</code></pre><p>AFTER: assignee rules</p>`,
		},
		{
			// The same marker CARRYING the delimiter: both renderers read
			// `2) ```a` under a prose line as paragraph text, delimiters and
			// all, which makes the column-0 delimiter below it the page's
			// opener and everything after it code. A reading that took the
			// marker's line for an opener would pair the two and call the
			// text balanced.
			name: "an ordered marker that cannot interrupt a paragraph opens no fence either",
			src:  "prose the bundle wrote\n2) ```a\n    x\n   ```\nAFTER: assignee rules\n",
			open: true,
			why:  "<p>prose the bundle wrote<br>2) ```a<br>x</p><pre><code>AFTER: assignee rules</code></pre>",
		},
		{
			// Its twin, one blank line apart: there the item exists, the
			// fence is its first block and the three-column delimiter closes
			// it, so nothing is left open. The negative control that keeps
			// the rule above from refusing every marker-carried opener.
			name: "after a blank line the same marker carries a real opener it closes",
			src:  "prose the bundle wrote\n\n2) ```a\n    x\n   ```\nAFTER: assignee rules\n",
			open: false,
			why:  `<ol start="2"><li><pre lang="a"><code> x</code></pre></li></ol><p>AFTER: assignee rules</p>`,
		},
		{
			// A FENCED BLOCK ends the paragraph above it as surely as a blank
			// line does, so the same marker under its closer opens a real
			// item — whose fence the three-column delimiter closes, leaving
			// nothing open. A reading carrying the paragraph across the block
			// would take the marker's line for prose and escape this closer.
			name: "a closed block leaves no paragraph open, so the marker below it opens an item",
			src:  "prose the bundle wrote\n```\nx\n```\n2) ```a\n    y\n   ```\nAFTER: assignee rules\n",
			open: false,
			why:  `<p>prose the bundle wrote</p><pre><code>x</code></pre><ol start="2"><li><pre lang="a"><code> y</code></pre></li></ol><p>AFTER: assignee rules</p>`,
		},
		{
			// Trailing whitespace after a closing run is not content: both
			// renderers close the block on it. Which is why an embedder owes
			// a field's lines no trimming — the trailing spaces it would
			// take are a hard line break, or the bytes of the author's own
			// diff example (pkg/botregistry.renderCatalogCard).
			name: "a closer with trailing spaces closes",
			src:  "```a\nx\n```   \nAFTER: assignee rules\n",
			open: false,
			why:  `<pre lang="a"><code>x</code></pre><p>AFTER: assignee rules</p>`,
		},
		{
			// A delimiter four or more columns in cannot open a block at the
			// page's own column, so a container holds it, and a column-0
			// line is outside that container whatever its content column.
			name: "a six-column delimiter is held by a container a column-0 line ends",
			src:  "What it posts:\n      ```suggestion\nthe replacement line\n```\nAFTER: assignee rules\n",
			open: true,
			why:  "<p>What it posts:<br>```suggestion<br>the replacement line</p><pre><code>AFTER: assignee rules</code></pre>",
		},
		{
			// What bounds a deep delimiter's container: CommonMark gives a
			// fence at most three columns past its container's content
			// column, so a six-column delimiter is held by one indenting at
			// least three — and a line four columns in is INSIDE that
			// container, not past its end. Both renderers pair the two
			// delimiters here (as an inline span, the delimiter being
			// paragraph text) and leave nothing open; a rule reading the
			// container's column off the delimiter's own would end the
			// container on the four-column line and call the text open.
			name: "a deep delimiter's container holds a line four columns in",
			src:  "What it posts:\n      ```suggestion\n    the replacement line\n    ```\nAFTER: assignee rules\n",
			open: false,
			why:  "<p>What it posts:<br><code>suggestion the replacement line </code><br>AFTER: assignee rules</p>",
		},
		{
			// The negative control of the four above: a container that keeps
			// its content closes its own fence, and the rule must not read
			// the end of the text into the middle of an item.
			name: "a fence a list item opens and closes inside itself is balanced",
			src:  "- ```suggestion\n  the replacement line\n  ```\n",
			open: false,
			why:  `<ul><li><pre lang="suggestion"><code>the replacement line</code></pre></li></ul>`,
		},
		{
			name: "a fence a blockquote opens and closes inside itself is balanced",
			src:  "> ```sh\n> run it\n> ```\n",
			open: false,
			why:  `<blockquote><div class="highlight highlight-source-shell"><pre>run it</pre></div></blockquote>`,
		},
		{
			// A blank line ends no container: both renderers keep the item
			// over it and the fence with it. A rule that closed a block on a
			// blank line would read every paragraph break as the item's end.
			name: "a blank line ends no container, so the fence survives it",
			src:  "- ```suggestion\n\n  still inside\n  ```\n",
			open: false,
			why:  `<ul><li><pre lang="suggestion"><code>` + "\n" + `still inside</code></pre></li></ul>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f mdcode.FenceScanner
			for _, line := range mdcode.Lines(tc.src) {
				f.Code(line)
			}
			if f.Open() != tc.open {
				t.Errorf("fence open at end = %v, the renderer says %v: %s\n  src %q", f.Open(), tc.open, tc.why, tc.src)
			}
		})
	}
}

// The container rule has a second edge, and it is the one that decides what
// internal/docsguard calls a LINK. A page reads through Mask, so a line's
// reading says whether a `](target)` on it is a link the page makes or a
// form it quotes — and a fence six columns in, inside an item whose content
// column is four, has its content at FOUR: inside the container, inside the
// block. Both renderers show that content as code and the column-0 line
// after the closer as prose (GitHub's /markdown API and pandoc 3.1.3, gfm
// and commonmark; r3v2/oracle6.log).
//
// So the container's column is read three columns back from a deep
// delimiter, not off the delimiter itself: reading it off the delimiter ends
// the item on its own content line and hands the quoted form to the link
// index, which is the failure that aborts `iterion map gen` on a page whose
// links are sound.
//
// The mutation that reddens this: return indent instead of indent - 3 in
// containerColumn.
func TestAFenceDeepInsideAnItemKeepsItsContentCode(t *testing.T) {
	const body = "1.  item text\n" +
		"      ```sh\n" +
		"    never write ](../../elsewhere/foo.md) in a page\n" +
		"    ```\n" +
		"AFTER: assignee rules\n"
	lines := mdcode.Lines(body)
	masked := mdcode.Lines(mdcode.Mask(body))
	if len(masked) != len(lines) {
		t.Fatalf("the mask walks %d lines and the source %d", len(masked), len(lines))
	}
	if strings.TrimSpace(masked[2]) != "" {
		t.Errorf("line 3 %q reads as PROSE: it is the fenced block's content, inside the item, and both renderers show it as code — the link form on it would be indexed as a link the page makes\n  mask %q", lines[2], masked[2])
	}
	if strings.Contains(mdcode.Mask(body), "elsewhere/foo.md") {
		t.Error("the quoted link form survived the mask — a scanner over it reads a link the page only showed")
	}
	if strings.TrimSpace(masked[4]) == "" {
		t.Errorf("line 5 %q reads as CODE: the closer at the item's content column closed the block, and the page shows this line as prose", lines[4])
	}
}

// Lines is the package's line rule, and the rule is a renderer's: a line ends
// at a line feed, a carriage return, or the two together. Everything that
// decides structure line by line goes through it, so a caller cannot reach
// for strings.Split and read a lone carriage return as an ordinary byte.
func TestLinesSplitsOnEveryCommonMarkLineEnding(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{name: "the empty text is one empty line", src: "", want: []string{""}},
		{name: "no line ending at all", src: "a line", want: []string{"a line"}},
		{name: "line feeds", src: "a\nb", want: []string{"a", "b"}},
		{name: "a trailing line feed yields a last empty line", src: "a\n", want: []string{"a", ""}},
		{name: "carriage returns and line feeds", src: "a\r\nb", want: []string{"a", "b"}},
		{name: "lone carriage returns", src: "a\rb", want: []string{"a", "b"}},
		{name: "the three mixed", src: "a\rb\r\nc\nd", want: []string{"a", "b", "c", "d"}},
		{name: "a carriage return at the very end", src: "a\r", want: []string{"a", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mdcode.Lines(tc.src)
			if len(got) != len(tc.want) {
				t.Fatalf("Lines(%q) = %q, want %q", tc.src, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Lines(%q) = %q, want %q", tc.src, got, tc.want)
				}
			}
		})
	}
	// Mask reads the same lines and keeps every offset, which is what lets a
	// caller read the bytes it found out of the original. The fixture is a
	// DANGLING opener, because that is where the two readings differ
	// visibly: a renderer takes every line after it into the block, while a
	// walk that never breaks the line sees one paragraph holding a single
	// unpaired backtick run — literal text by the span rule, so the page
	// below reads as prose and a quoted link form in it is indexed as a
	// link.
	const src = "prose\r```yaml\rnodes: []\rand on"
	masked := mdcode.Mask(src)
	if len(masked) != len(src) {
		t.Fatalf("Mask changed the length: %d vs %d", len(masked), len(src))
	}
	for _, code := range []string{"nodes: []", "and on"} {
		i := strings.Index(src, code)
		if strings.TrimSpace(masked[i:i+len(code)]) != "" {
			t.Errorf("Mask reads %q as prose — the block the carriage return opened holds it: %q", code, masked)
		}
	}
	// And the prose BEFORE the opener stays prose, so the mask is not simply
	// blanking the whole text.
	if masked[:len("prose")] != "prose" {
		t.Errorf("Mask blanked the prose above the opener: %q", masked)
	}
}

// An info string carrying a backtick means the line is a paragraph. Asserted
// through the scanner, which is the only way this rule is reachable now.
func TestAnInfoStringWithABacktickOpensNothing(t *testing.T) {
	var f mdcode.FenceScanner
	if f.Code("```go `x`") || f.Open() {
		t.Error("a paragraph that looks like a fence opener opened one")
	}
	if !f.Code("```go") || !f.Open() {
		t.Error("a genuine opener no longer opens")
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

// openAtEnd reports whether md leaves a fenced block open — the state the
// text imposes on whatever is spliced after it. It reads the lines a renderer
// reads (mdcode.Lines), so a break the text carries as a carriage return
// counts here as it counts on the page.
func openAtEnd(md string) bool {
	var fence mdcode.FenceScanner
	for _, line := range mdcode.Lines(md) {
		fence.Code(line)
	}
	return fence.Open()
}

// EscapeDanglingFence's contract is what an embedder owes the document
// around it: whatever the text ships, the document's fence state after it is
// the one before it. Each row is a form the corpus writes.
func TestEscapeDanglingFenceClosesWhatEmbeddingWouldLeak(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{
			name: "a word wrap onto a fence line",
			src:  "inline comments with one-click\n```suggestion blocks (GitHub). Never edits.",
			want: "inline comments with one-click\n\\`\\`\\`suggestion blocks (GitHub). Never edits.",
		},
		{
			name: "a tilde opener",
			src:  "a wrap onto\n~~~suggestion and the rest",
			want: "a wrap onto\n\\~\\~\\~suggestion and the rest",
		},
		{
			name: "an opener a blockquote pushed off column 0",
			src:  "quoted prose\n>  ```md and the rest",
			want: "quoted prose\n>  \\`\\`\\`md and the rest",
		},
		{
			name: "an opener a list marker pushed off column 0",
			src:  "a list:\n- ```md and the rest",
			want: "a list:\n- \\`\\`\\`md and the rest",
		},
		{
			// The example indented under the bullet that introduces it, with
			// the closing delimiter back at the bullet's column: the item
			// ends there and closes the block with it, so the author's
			// CLOSER is the page's opener, and it is the one the escape must
			// land on. The opener above it stays whole — the item still
			// shows the example.
			name: "the closer below an item is the page's opener, and the escape lands on it",
			src:  "- on a PR it posts, for each finding:\n  ```suggestion\n  the replacement line\n```",
			want: "- on a PR it posts, for each finding:\n  ```suggestion\n  the replacement line\n\\`\\`\\`",
		},
		{
			name: "a fence an item opens and closes inside itself is the author's and stays whole",
			src:  "- on a PR it posts, for each finding:\n  ```suggestion\n  the replacement line\n  ```",
			want: "- on a PR it posts, for each finding:\n  ```suggestion\n  the replacement line\n  ```",
		},
		{
			// The same shape under a marker that opens no list where it sits
			// (an ordered one starting elsewhere than 1, under a prose line):
			// the fence is the PAGE's own, the column-0 delimiter is its
			// closer, and escaping that closer would leave the opener live —
			// a phantom fence written by the repair. Nothing to repair here,
			// so nothing is touched.
			name: "a fence below a marker that opens no list is balanced and untouched",
			src:  "prose the bundle wrote\n2) a step\n   ```a\n   x\n```",
			want: "prose the bundle wrote\n2) a step\n   ```a\n   x\n```",
		},
		{
			// And its twin after a blank line, where the marker DOES open an
			// item: the column-0 delimiter is the page's opener and takes
			// the escape.
			name: "after a blank line the same marker holds the fence, and the escape lands below it",
			src:  "prose the bundle wrote\n\n2) a step\n   ```a\n   x\n```",
			want: "prose the bundle wrote\n\n2) a step\n   ```a\n   x\n\\`\\`\\`",
		},
		{
			name: "a balanced block is the author's and stays whole",
			src:  "reads:\n\n```yaml\nnodes: []\n```\n\nand runs it.",
			want: "reads:\n\n```yaml\nnodes: []\n```\n\nand runs it.",
		},
		{
			name: "a dangling opener AFTER a balanced block",
			src:  "```yaml\nnodes: []\n```\nthen a wrap onto\n```suggestion blocks",
			want: "```yaml\nnodes: []\n```\nthen a wrap onto\n\\`\\`\\`suggestion blocks",
		},
		{
			// The first escape promotes a line the open block was HOLDING as
			// content into an opener of its own: one pass is not enough.
			name: "a line the escaped block held as content becomes an opener",
			src:  "```one\n```two",
			want: "\\`\\`\\`one\n\\`\\`\\`two",
		},
		{
			// A tab is four columns, so the closer the author indented with
			// one closes nothing and the block above it is dangling. Both
			// delimiters end up neutralised: the second line opens a block
			// of its own once the first is escaped.
			name: "a closer a tab pushed four columns in closes nothing",
			src:  "look:\n```suggestion\n- the replacement\n\t```",
			want: "look:\n\\`\\`\\`suggestion\n- the replacement\n\t\\`\\`\\`",
		},
		{
			// A lone carriage return is a line break for a renderer, so the
			// run after it is at column 0 and opens a block. The text comes
			// back with line feeds: the lines the embedder, this package and
			// the page read are then the same lines.
			name: "a wrap onto a fence line after a carriage return",
			src:  "inline comments with one-click\r```suggestion blocks (GitHub).",
			want: "inline comments with one-click\n\\`\\`\\`suggestion blocks (GitHub).",
		},
		{
			name: "a balanced block delimited by carriage returns stays whole",
			src:  "reads:\r\r```yaml\rnodes: []\r```\r\rand runs it.",
			want: "reads:\n\n```yaml\nnodes: []\n```\n\nand runs it.",
		},
		{
			name: "carriage returns and line feeds are one line ending each",
			src:  "a wrap onto\r\n```suggestion and the rest",
			want: "a wrap onto\n\\`\\`\\`suggestion and the rest",
		},
		{
			name: "no fence at all is returned byte for byte",
			src:  "plain prose with an inline `span` and a stray ` backtick",
			want: "plain prose with an inline `span` and a stray ` backtick",
		},
		{
			name: "the empty text",
			src:  "",
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mdcode.EscapeDanglingFence(tc.src)
			if got != tc.want {
				t.Errorf("EscapeDanglingFence(%q)\n  got  %q\n  want %q", tc.src, got, tc.want)
			}
			if openAtEnd(got) {
				t.Errorf("EscapeDanglingFence(%q) still leaves a fenced block open: %q", tc.src, got)
			}
			if again := mdcode.EscapeDanglingFence(got); again != got {
				t.Errorf("EscapeDanglingFence is not idempotent on %q: %q", got, again)
			}
		})
	}
}

// The escape neutralises the delimiter run WHOLE, so what it leaves behind
// is literal text for a renderer as well as for Mask. CommonMark consumes
// one backtick per backslash: a single backslash would leave a run of n-1,
// which is still a code-span delimiter. Measured with pandoc 3.1.3
// (commonmark, gfm and commonmark_x alike): one backslash before the three
// backticks of this very field renders the sentence after them as CODE and
// eats the author's own two-backtick span.
func TestEscapeDanglingFenceLeavesNoLiveDelimiter(t *testing.T) {
	// The field as bots/review-pr/manifest.yaml shipped it before the
	// rewrap: a wrap onto a fence line, and the author's own span after it.
	const src = "one-click\n" +
		"```suggestion blocks (GitHub / GitLab / Forgejo).\n" +
		"Never edits ``code`` itself."
	escaped := mdcode.EscapeDanglingFence(src)

	// Every character of the run carries its own backslash, so no run of
	// width > 1 is left for a renderer to pair.
	opener := strings.Split(escaped, "\n")[1]
	if !strings.HasPrefix(opener, "\\`\\`\\`suggestion") {
		t.Errorf("the delimiter run is not escaped character by character: %q", opener)
	}
	for _, run := range []string{"``", "~~"} {
		if i := strings.Index(opener, run); i >= 0 {
			t.Errorf("a delimiter run of width %d survives at byte %d of %q — a renderer pairs it with the next run of that width",
				len(run), i, opener)
		}
	}

	// And the prose stays prose. Byte by byte: everything Mask blanks is
	// either a byte the escape wrote or a byte of the span the field itself
	// wrote — no word of the text between them.
	masked := mdcode.Mask(escaped)
	if len(masked) != len(escaped) {
		t.Fatalf("Mask changed the length: %d vs %d", len(masked), len(escaped))
	}
	const span = "``code``"
	spanAt := strings.Index(escaped, span)
	escapeAt := strings.Index(escaped, "\\`")
	if spanAt < 0 || escapeAt < 0 {
		t.Fatalf("the escaped field lost a landmark this test measures against: %q", escaped)
	}
	escapeEnd := escapeAt + len("\\`\\`\\`")
	for i := range escaped {
		if escaped[i] == masked[i] {
			continue
		}
		inEscape := i >= escapeAt && i < escapeEnd
		inSpan := i >= spanAt && i < spanAt+len(span)
		if !inEscape && !inSpan {
			t.Errorf("Mask blanked byte %d (%q) — neither the escape nor the field's own span, so the escape left a delimiter that reads prose as code: %q",
				i, escaped[i], escaped)
		}
	}
	if masked[spanAt] == escaped[spanAt] {
		t.Errorf("the field's own ``code`` span is no longer read as a span: %q", masked)
	}
}

// Escaping one opener promotes the line the open block was HOLDING into an
// opener of its own, so the walk repeats — and it has to repeat as many times
// as the text has openers to promote. A text of k such lines must come back
// with k escapes and nothing open, whatever k is: the walk that stops after
// the first one leaves k-1 phantom fences in the document it was repairing.
//
// What this does NOT witness is the resume (`from = at + 1`): restarting at
// the first line produces the SAME output, and the measured cost is the
// lookahead to the END of the text rather than the prefix (one pass per
// dangling opener, 35 s at 20 000 of them), so no output and no wall clock
// can tell the two apart. The bound below is a smoke bound on the common
// shape — a long field with one wrap accident — and says so.
func TestEscapeDanglingFenceEscapesEveryPromotedOpener(t *testing.T) {
	for _, k := range []int{1, 2, 5, 50} {
		// Each line opens a block the next line cannot close (its delimiter
		// carries an info string), so the last one is dangling and every
		// earlier one is promoted by the escape before it.
		field := strings.TrimSuffix(strings.Repeat("```info\n", k), "\n")
		out := mdcode.EscapeDanglingFence(field)
		if got := strings.Count(out, "\\`\\`\\`info"); got != k {
			t.Errorf("a text of %d promoted openers came back with %d escape(s): %q", k, got, out)
		}
		if openAtEnd(out) {
			t.Errorf("a text of %d promoted openers still leaves a fenced block open: %q", k, out)
		}
		if again := mdcode.EscapeDanglingFence(out); again != out {
			t.Errorf("not idempotent on %d promoted openers: %q", k, again)
		}
	}

	// The shape a bundle ships: ordinary prose word-wrapped onto one fence
	// line. A pass that read the whole text again for every line it settled
	// would take minutes here.
	const n = 20_000
	const budget = 10 * time.Second
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("a line of ordinary prose\n")
	}
	b.WriteString("```suggestion")
	start := time.Now()
	out := mdcode.EscapeDanglingFence(b.String())
	elapsed := time.Since(start)
	if got := strings.Count(out, "\\`\\`\\`suggestion"); got != 1 {
		t.Fatalf("escaped %d opener(s) in the %d-line field, want 1", got, n)
	}
	if openAtEnd(out) {
		t.Fatalf("the escaped text still leaves a fenced block open")
	}
	if elapsed > budget {
		t.Errorf("escaping one opener in a %d-line text took %s, over the %s budget",
			n, elapsed.Round(time.Millisecond), budget)
	}
}

// The repair must land on the delimiter the RENDERER reads as the opener of
// the block nothing closes — which, where a container held the first one, is
// the SECOND delimiter, not the first. The two are on different lines and at
// different offsets, so the claim is a byte offset: the escape rewrites that
// line and leaves the author's other delimiter alone.
//
// It is the one shape where a single line both CLOSES the block its container
// held and OPENS one of its own, so a scan that compared "open before" with
// "open after" would see no change on it, keep blaming the opener the
// container already closed, and write its backslashes into the words of a
// line the page shows as prose while the real opener stayed live.
//
// Measured, GitHub's /markdown API and pandoc 3.1.3 (gfm and commonmark)
// alike: for the text below the page shows an EMPTY ```suggestion block
// inside the bullet, "the replacement line" as prose, and everything from
// the column-0 delimiter on as code (r3v2/gh-oracle4.log, oracle4.log).
func TestTheEscapeLandsOnTheDelimiterTheRendererOpens(t *testing.T) {
	for _, tc := range []struct{ name, src, wantLine string }{
		{
			name:     "a bullet holds the first delimiter",
			src:      "What it posts:\n- ```suggestion\nthe replacement line\n```\nAFTER: assignee rules\n",
			wantLine: "\\`\\`\\`",
		},
		{
			name:     "a numbered item holds the first delimiter",
			src:      "What it posts:\n1. ```suggestion\nthe replacement line\n```\nAFTER: assignee rules\n",
			wantLine: "\\`\\`\\`",
		},
		{
			name:     "a blockquote holds the first delimiter",
			src:      "Note:\n> ```sh\n> run it\n```\nAFTER: assignee rules\n",
			wantLine: "\\`\\`\\`",
		},
		{
			name:     "six columns of indentation hold the first delimiter",
			src:      "What it posts:\n      ```suggestion\nthe replacement line\n```\nAFTER: assignee rules\n",
			wantLine: "\\`\\`\\`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := mdcode.EscapeDanglingFence(tc.src)
			lines := mdcode.Lines(out)
			if got := lines[3]; got != tc.wantLine {
				t.Errorf("line 4 of the escaped text is %q, want %q — the repair did not land on the delimiter the page reads as the dangling opener:\n%s", got, tc.wantLine, out)
			}
			// And the author's own delimiter, the one a container holds, is
			// returned byte for byte: escaping it would put backslashes in
			// the words of a line the page shows as an ordinary code block.
			if got, want := lines[1], mdcode.Lines(tc.src)[1]; got != want {
				t.Errorf("the delimiter the container holds was rewritten: %q, want %q", got, want)
			}
			if n := strings.Count(out, "\\`"); n != 3 {
				t.Errorf("%d backslash(es) written, want 3 — one delimiter, every character of it:\n%s", n, out)
			}
			if openAtEnd(out) {
				t.Errorf("the escaped text still leaves a fenced block open:\n%s", out)
			}
			if again := mdcode.EscapeDanglingFence(out); again != out {
				t.Errorf("not idempotent: %q", again)
			}
		})
	}
}

// Cell is what a value owes the ROW it is spliced into. Inline keeps it on
// one line; the pipe escape keeps it in its own column — a cell is cut on its
// unescaped pipes before any of its text is read, so an unescaped one in a
// persona pushes that persona's tail into the column after it and drops the
// row's last cell off the table.
func TestCellKeepsAValueInsideItsOwnColumn(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"text with no pipe and no break is returned as written", "Revi the reviewer", "Revi the reviewer"},
		{"a pipe is escaped", "Revi | the reviewer", `Revi \| the reviewer`},
		{"every pipe is escaped", "a|b|c", `a\|b\|c`},
		{"a pipe inside a code span is escaped too", "the `a|b` form", "the `a\\|b` form"},
		{"a line feed is collapsed", "Revi\n```suggestion", "Revi ```suggestion"},
		{"a carriage return is collapsed", "Revi\r```suggestion", "Revi ```suggestion"},
		{"both at once", "Revi | the\nreviewer", `Revi \| the reviewer`},
		{"the empty value", "", ""},
		// A pipe the TEXT already escaped is a separator for nobody, and
		// escaping it again writes `\\|` — an escaped backslash and a LIVE
		// separator, the cell cut in two by its own repair.
		{"a pipe the text already escaped is left alone", `Revi \| the reviewer`, `Revi \| the reviewer`},
		{"inside a code span too", "the `a\\|b` form", "the `a\\|b` form"},
		// An EVEN run before the pipe escapes the backslash, not the pipe:
		// the pipe is live and takes its escape.
		{"an escaped backslash before a live pipe", `Revi \\| the reviewer`, `Revi \\\| the reviewer`},
		{"a trailing backslash is not an escape of anything", `a backslash \`, `a backslash \`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mdcode.Cell(tc.src); got != tc.want {
				t.Errorf("Cell(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
	// The row the escape protects, as the renderers read it (GitHub's
	// /markdown API and pandoc 3.1.3 gfm, 2026-10-12; the already-escaped
	// value 2026-10-13): each form renders three cells with the pipe shown
	// inside the first one, where the bare form renders "the reviewer" under
	// the second heading and loses the third value entirely.
	//
	// Counted by the ROW's own rule — a pipe is a separator when the run of
	// backslashes before it is even. A `[^\\]` lookbehind cannot see the
	// difference between `\|` and `\\|`, and that is precisely the byte this
	// test exists to catch.
	for _, value := range []string{"Revi | the reviewer", `Revi \| the reviewer`, `Revi \\| the reviewer`} {
		row := "| " + mdcode.Cell(value) + " | `rev` | verify |"
		if n := separatorPipes(row); n != 4 {
			t.Errorf("Cell(%q): the row carries %d cell separator(s), a three-column row carries 4: %s", value, n, row)
		}
	}
}

// separatorPipes counts the pipes of a row that CUT it into cells: a pipe is
// a separator when the run of backslashes immediately before it is even.
func separatorPipes(row string) int {
	n, slashes := 0, 0
	for i := 0; i < len(row); i++ {
		switch {
		case row[i] == '|' && slashes%2 == 0:
			n++
			slashes = 0
		case row[i] == '\\':
			slashes++
		default:
			slashes = 0
		}
	}
	return n
}

// ReplaceOutsideCode is the pattern every scanner of this tree runs — a
// grammar over the mask, the bytes read out of the original — with the rule
// that is easy to leave out: a window the mask WIDENED is not a match the
// document wrote, and rewriting it loses the bytes the mask blanked.
func TestReplaceOutsideCodeRewritesOnlyWhatTheMaskCallsProse(t *testing.T) {
	linkRe := regexp.MustCompile(`\]\(([^)\n]*)\)`)
	upper := func(m string) string { return "](REWRITTEN)" }
	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{
			name: "a destination in prose is rewritten",
			src:  "see [the page](../docs/x.md) for more",
			want: "see [the page](REWRITTEN) for more",
		},
		{
			name: "a destination inside a fenced block is left whole",
			src:  "```md\n[the page](../docs/x.md)\n```\n",
			want: "```md\n[the page](../docs/x.md)\n```\n",
		},
		{
			name: "a destination inside an inline span is left whole",
			src:  "an example: `[the page](../docs/x.md)`",
			want: "an example: `[the page](../docs/x.md)`",
		},
		{
			name: "a window the mask widened keeps its bytes",
			src:  "a link [x](a`)`/../b) in prose",
			want: "a link [x](a`)`/../b) in prose",
		},
		{
			name: "no match is returned byte for byte",
			src:  "plain prose with a stray ]( and nothing else",
			want: "plain prose with a stray ]( and nothing else",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mdcode.ReplaceOutsideCode(tc.src, linkRe, upper); got != tc.want {
				t.Errorf("ReplaceOutsideCode(%q)\n  got  %q\n  want %q", tc.src, got, tc.want)
			}
		})
	}
}
