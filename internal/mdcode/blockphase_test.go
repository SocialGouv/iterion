package mdcode

import (
	"strings"
	"testing"
)

// The ticket's residual class, written down as witnesses: the shapes the
// catalogue-fence review measured leaking past the line-by-line reading,
// with pandoc 3.1.3 (gfm) as the oracle through the real generator. The
// review's sweep enumerated 14 580 adversarial field shapes and counted 123
// leaking — every tuple below is one of those 123, spelled exactly as the
// sweep generated it (context | deep delimiter | filler | shallow one), and
// the last two members come from the same class where the sweep's corpus
// does not reach it: a container the line-by-line reading cannot see at all
// (the lazy paragraph's item, the GFM table's item).
//
// For every member: the source gate must refuse it (a fence the text opens
// and never closes), the generator's repair must land on the delimiter the
// page reads as the phantom's opener, and the result must leave nothing
// open and survive its own re-application. The block phase repairs every
// one of the 123 — leaking=0 in the same sweep, the ticket's guarantee (3).
func TestBlockPhaseClosesTheResidualClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"prose|c4|x2|c1", "prose the bundle wrote\n    ```\n  x\n ```"},
		{"prose-blank|c4|blank|c2", "prose the bundle wrote\n\n    ```\n\n  ```"},
		{"prose|c6|x4|c3", "prose the bundle wrote\n      ```\n    x\n   ```"},
		{"prose|tab|blank|c1", "prose the bundle wrote\n\t```\n\n ```"},
		{"prose|t4|x2|t2", "prose the bundle wrote\n    ~~~\n  x\n  ~~~"},
		{"prose|sptab|x4|c3", "prose the bundle wrote\n \t```\n    x\n   ```"},
		{"item|c4|blank|c1", "- an item the bundle wrote\n    ```\n\n ```"},
		{"item|tab|x4|c1", "- an item the bundle wrote\n\t```\n    x\n ```"},
		{"item|sptab|x2|c1", "- an item the bundle wrote\n \t```\n  x\n ```"},
		{"quote|c4|x4|c1", "> a quote the bundle wrote\n    ```\n    x\n ```"},
		{"quote|sptab|blank|c2", "> a quote the bundle wrote\n \t```\n\n  ```"},
		{"quote|c5|x2|c3", "> a quote the bundle wrote\n     ```\n  x\n   ```"},
		{"the same class under a lazy paragraph's item", "- an item the bundle wrote\nthen prose continuing it\n  ```a\n  x\n```"},
		{"the same class under a table's item", "| a | b |\n|---|---|\n| x | y |\n2) a step\n   ```a\n   x\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f FenceScanner
			for _, line := range Lines(tc.src) {
				f.Code(line)
			}
			if !f.Open() {
				t.Errorf("the gate reads the residual member as balanced — it must refuse it:\n%s", tc.src)
			}
			got := EscapeDanglingFence(tc.src)
			if got == tc.src {
				t.Errorf("the generator's repair did not fire:\n%s", tc.src)
			}
			var g FenceScanner
			for _, line := range Lines(got) {
				g.Code(line)
			}
			if g.Open() {
				t.Errorf("the repaired text still leaves a fence open:\n%s", got)
			}
			if again := EscapeDanglingFence(got); again != got {
				t.Errorf("not idempotent:\n%s", again)
			}
		})
	}
}

// Seventeen of the review's nineteen legitimate balanced shapes — the ones
// both renderers read as sound code blocks — stay sound: the gate accepts
// them (nothing left open), and the generator's repair touches none of
// them. The two the sweep declared outside its reach (f1-item-above-closed-inside,
// ord2-above-prose-legit) ride in the review's TestR4TShapes instead. These
// seventeen are the shapes the three refuted gate candidates refused, which
// is why no fourth spelling of a gate was written.
func TestTheLegitimateBalancedShapesStaySound(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"legit-col0", "```\nkeep me as code\n```"},
		{"legit-col0-info", "```suggestion\nkeep me as code\n```"},
		{"legit-indent2-closer-indent2", "  ```a\n  keep me as code\n  ```"},
		{"legit-indent2-closer-col0", "  ```a\n  keep me as code\n```"},
		{"legit-indent3-closer-col0", "   ```a\n   keep me as code\n```"},
		{"legit-col0-closer-indent3", "```a\nkeep me as code\n   ```"},
		{"legit-4col-balanced", "prose the bundle wrote\n\n    ```a\n    keep me as code\n    ```"},
		{"legit-tilde", "~~~\nkeep me as code\n~~~"},
		{"legit-long-run", "`````\nkeep me as code\n`````"},
		{"legit-quote", "> ```\n> keep me as code\n> ```"},
		{"legit-item-marker-on-delimiter", "- ```a\n  keep me as code\n  ```"},
		{"legit-nested-item", "- outer:\n  - inner:\n    ```a\n    keep me as code\n    ```"},
		{"legit-closer-trailing-spaces", "```a\nkeep me as code\n```   "},
		{"legit-after-hardbreak", "a line ending in two spaces  \n```a\nkeep me as code\n```"},
		{"legit-two-blocks", "```a\nkeep me as code\n```\n\nand then\n\n```b\nand me too\n```"},
		{"legit-crlf", "prose\r\n```a\r\nkeep me as code\r\n```"},
		{"legit-prose-only", "ordinary prose with `a span` and a [link](x.md)."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f FenceScanner
			for _, line := range Lines(tc.src) {
				f.Code(line)
			}
			if f.Open() {
				t.Errorf("the gate refuses a shape both renderers read as sound:\n%s", tc.src)
			}
			// The repair must not fire. The one renormalisation it owns is
			// the line endings' (a lone carriage return is a break for a
			// renderer and invisible to a byte compare), so the claim is
			// "no delimiter rewritten", byte for byte, on the lines.
			got := EscapeDanglingFence(tc.src)
			if strings.Count(got, `\~`) != strings.Count(tc.src, `\~`) ||
				strings.Count(got, "\\`") != strings.Count(tc.src, "\\`") {
				t.Errorf("the repair rewrote a deliberate block:\n  got  %q\n  from %q", got, tc.src)
			}
			if want := strings.Join(Lines(tc.src), "\n"); got != want {
				t.Errorf("the repair changed the text beyond its line-ending renormalisation:\n  got  %q\n  want %q", got, want)
			}
		})
	}
}

// The block phase's own decisions, witnessed where the line-by-line reading
// had no answer. Each case carries the renderer's answer in its comment —
// pandoc 3.1.3 gfm, asked during the ticket's review — because these are
// the shapes a future simplification will be tempted to "fix". The `why`
// strings are a GitHub-style HTML spelling, NOT what `pandoc -t html`
// prints (`<pre lang="a">` where pandoc writes `<pre class="a">`, a soft
// break as `<br>` where pandoc writes a space): a paste-compare against
// pandoc's own output sees spurious diffs. The tests assert Open(), never
// the strings.
func TestTheBlockPhaseReadsTheContainersItTracks(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		open bool
		why  string
	}{
		{
			// An empty item does not survive the blank line after it
			// (a list item can begin with at most one blank line), so the
			// indented delimiter is the document's own fence, and the
			// column-0 line closes it.
			name: "an empty item dies on the blank line, so the fence below is the page's",
			src:  "-\n\n  ```a\n  x\n```",
			open: false,
			why:  "<ul><li></li></ul><pre lang=\"a\"><code>x</code></pre>",
		},
		{
			name: "a fence interrupts a quote's paragraph and opens outside it",
			src:  "> para\n```\nfence\n```\n",
			open: false,
			why:  "<blockquote><p>para</p></blockquote><pre><code>fence</code></pre>",
		},
		{
			name: "a fence interrupts a lazily continued paragraph at column 0",
			src:  "- an item\nthen prose continuing it\n```\nfence\n```\n",
			open: false,
			why:  "<ul><li>an item<br>then prose continuing it</li></ul><pre><code>fence</code></pre>",
		},
		{
			// The deeper quote marker is fence CONTENT, not a closer: the
			// fence lives in the outer quote, and the second `>` past it is
			// text the page shows inside the block (the content pandoc
			// renders is "x\n> ```"). The block ends with its container:
			// the line after the quote is prose.
			name: "a deeper quote marker inside a blockquote fence is content",
			src:  "> ```\n> x\n>> ```\nafter",
			open: false,
			why:  "<blockquote><pre><code>x\\n&gt; ```</code></pre></blockquote><p>after</p>",
		},
		{
			// A delimiter row under a line with no pipe is paragraph text
			// (no table), so the marker below it cannot interrupt and the
			// three-column delimiter is the page's opener.
			name: "a delimiter row under a pipe-less line is no table",
			src:  "prose\n|---|---|\nmore\n   ```a\n   x\n```",
			open: false,
			why:  "<p>prose<br>|---|---|<br>more</p><pre lang=\"a\"><code>x</code></pre>",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f FenceScanner
			for _, line := range Lines(tc.src) {
				f.Code(line)
			}
			if f.Open() != tc.open {
				t.Errorf("fence open at end = %v, the renderer says %v: %s\n  src %q", f.Open(), tc.open, tc.why, tc.src)
			}
		})
	}
}

// An empty list item that GAINS content stops being empty: the
// at-most-one-blank rule ends only an item that has received nothing yet,
// so the blank below carries the fence across and the next delimiter is
// the CONTENT of a fence with no closer — text the page shows. The empty
// flag, once set and never cleared, read that blank as the item's end: the
// fence died on it, the content delimiter opened a phantom block, and the
// line below read as code where pandoc shows prose. pandoc 3.1.3 gfm:
//
//   - [BulletList [ [ CodeBlock ("",["a"],[]) "c\n\n```b" ] ]
//     ```a           , Para "y"]
//     c
//
//     ```b
//     y
func TestAnItemThatTookContentKeepsItsFenceOverABlankLine(t *testing.T) {
	src := "-\n  ```a\n  c\n\n  ```b\ny\n"
	var f FenceScanner
	opened := 0
	var openedDelim string
	for _, line := range Lines(src) {
		f.Code(line)
		if f.openedOn {
			opened++
			openedDelim = line
		}
	}
	if opened != 1 || openedDelim != "  ```a" {
		t.Errorf("the walk read %d openers (last %q) — the content delimiter must open nothing:\n%s", opened, openedDelim, src)
	}
	if f.Open() {
		t.Errorf("the gate refuses a shape pandoc reads as one balanced block:\n%s", src)
	}
	if got := EscapeDanglingFence(src); got != strings.Join(Lines(src), "\n") {
		t.Errorf("the repair rewrote a balanced shape — the escape landed on live content:\n  got  %q", got)
	}
}

// A `>` cannot stand inside a thematic break: `- - > -` opens
// list>list>quote (the quote holding an empty item), not a rule with no
// container — and the fence below then lives INSIDE the surviving item,
// where the break reading put it in indented code and turned its content
// into prose. pandoc 3.1.3 gfm:
//
//   - - > -         [BulletList [ [ BulletList [ [ BlockQuote [
//     ```x            BulletList [[]], CodeBlock ("",["x"],[]) "c" ] ] ] ] ]
//     c                , Para "after"]
//     ```
//     after
//
// pandoc nests the fence one frame deeper than this reader (its quote
// continues without a marker over the empty item); the membership is the
// same: `c` is fence content, `after` is prose.
func TestABreakCannotCarryAQuoteMarker(t *testing.T) {
	src := "- - > -\n    ```x\n    c\n    ```\nafter\n"
	var f FenceScanner
	framesAfterLine0 := -1
	openedAt := -1
	i := -1
	for _, line := range Lines(src) {
		i++
		f.Code(line)
		if f.openedOn {
			openedAt = i
		}
		if i == 0 {
			framesAfterLine0 = len(f.frames)
		}
	}
	if framesAfterLine0 < 3 {
		t.Errorf("line 1 left %d frames — a break with no container:\n%s", framesAfterLine0, src)
	}
	if openedAt != 1 {
		t.Errorf("the fence opened at line %d, want the delimiter below the items (line 1):\n%s", openedAt, src)
	}
	if f.Open() {
		t.Errorf("the gate refuses a shape pandoc reads as one balanced block:\n%s", src)
	}
}

// A setext level-1 underline closes the paragraph above it — a heading
// opens nothing — so the ordered marker below it can open its item and the
// fence that item holds opens where pandoc reads one. Read as paragraph
// text, the underline kept the paragraph open: the marker was refused
// (an ordered start of 2 cannot interrupt), the frames diverged, and the
// fence membership below flipped away from the renderer. pandoc 3.1.3 gfm:
//
//	para            [Header 1 "para",
//	===              OrderedList (2,…) [ Plain "x",
//	2) x                                CodeBlock ("",["a"],[]) "" ],
//	   ```a          CodeBlock ("",["b"],[]) "closer\n"]
//	```b
//	closer
func TestASetextUnderlineClosesTheParagraph(t *testing.T) {
	src := "para\n===\n2) x\n   ```a\n```b\ncloser\n"
	var f FenceScanner
	var openedOn []int
	itemFrames := -1
	i := -1
	for _, line := range Lines(src) {
		i++
		f.Code(line)
		if f.paraOpen && i == 1 {
			t.Errorf("the underline left the paragraph open — the marker below it was refused:\n%s", src)
		}
		if f.openedOn {
			openedOn = append(openedOn, i)
		}
		if i == 2 {
			// the item the marker opens; it ends again at the outdented
			// delimiter, and the fence it holds ends with it
			itemFrames = len(f.frames)
		}
	}
	if itemFrames != 1 {
		t.Errorf("%d frames at the marker, want the one item it opened:\n%s", itemFrames, src)
	}
	if len(openedOn) != 2 || openedOn[0] != 3 || openedOn[1] != 4 {
		t.Errorf("the walk opened fences at lines %v, want the item's (3) and the one the outdented delimiter re-opens (4):\n%s", openedOn, src)
	}
	if !f.Open() {
		t.Errorf("the gate reads the shape as balanced — pandoc holds a live CodeBlock on closer:\n%s", src)
	}
}

// The escape's re-read carries the container state the escape point left
// open. A rescan that started past the escaped opener with a fresh scanner
// dropped the frames — the promoted delimiter a container still held read
// as indented text, the repair came back incomplete, and a second pass
// rewrote what the first had left: found by the round-1 fuzz (doc 124 of
// 400), minimised to three lines. pandoc 3.1.3 gfm, asked in the jail:
//
//  2. x            [OrderedList (2,…) [Plain "x"]                    — the fence opens
//     ~~~              , CodeBlock ("",[],[]) " ```\n"]                    inside nothing
//     ```
//
// — and the repaired text reads as ONE paragraph inside the item, no
// CodeBlock; a line spliced after it lands in that paragraph, prose.
// The resume replays the line that cut the container: skipping it (resuming
// AT the escape instead of before it) lets "2) x" re-open an item and escapes
// the lazy "    ```" line pandoc folds as paragraph text — rewriting author
// content. Exactly one delimiter moves; a resume that skips the cut line
// moves two.
func TestTheResumeReplaysTheLineThatCutTheQuote(t *testing.T) {
	src := "> ````\n~~~\n2) x\n    ```\n"
	var f FenceScanner
	for _, line := range Lines(src) {
		f.Code(line)
	}
	if !f.Open() {
		t.Fatalf("the gate reads the shape as balanced — it must refuse it:\n%s", src)
	}
	got := EscapeDanglingFence(src)
	if got == src {
		t.Fatalf("the repair did not fire:\n%s", src)
	}
	moved := 0
	for i, line := range strings.Split(got, "\n") {
		if i < len(strings.Split(src, "\n")) && strings.Split(src, "\n")[i] != line {
			moved++
		}
	}
	if moved != 1 {
		t.Errorf("the repair escaped %d lines, want exactly the one promoted delimiter:\n  from %q\n  got  %q", moved, src, got)
	}
	if line, _, d := danglingFenceOpener(Lines(got)); line >= 0 {
		t.Errorf("repair incomplete: a fresh full scan still reports a dangling opener at line %d (delim %q):\n%s", line, d, got)
	}
	if again := EscapeDanglingFence(got); again != got {
		t.Errorf("not idempotent:\n  got  %q\n  from %q", again, got)
	}
}

func TestTheEscapeCarriesTheContainerStackItResumesFrom(t *testing.T) {
	src := "2) x\n~~~\n    ```\n"
	var f FenceScanner
	for _, line := range Lines(src) {
		f.Code(line)
	}
	if !f.Open() {
		t.Errorf("the gate reads the shape as balanced — it must refuse it:\n%s", src)
	}
	got := EscapeDanglingFence(src)
	if got == src {
		t.Fatalf("the repair did not fire:\n%s", src)
	}
	// The repair's own promise, read off a fresh full scan: nothing left
	// open — neither the escaped opener nor the delimiter it promoted.
	if line, mark, d := danglingFenceOpener(Lines(got)); line >= 0 {
		t.Errorf("repair incomplete: a fresh full scan still reports a dangling opener at line %d (col %d, delim %q):\n%s", line, mark, d, got)
	}
	if again := EscapeDanglingFence(got); again != got {
		t.Errorf("not idempotent:\n  got  %q\n  from %q", again, got)
	}
	// What a caller's splice lands in: the embedder appends a line of its
	// own, and every reader must call it prose. pandoc: the paragraph of
	// the item carries it (SoftBreak "SPLICED PROSE LINE"), no CodeBlock.
	spliced := got + "SPLICED PROSE LINE\n"
	var g FenceScanner
	splicedIsProse := true
	for _, line := range Lines(spliced) {
		if g.Code(line) {
			splicedIsProse = false
		}
	}
	if !splicedIsProse || g.Open() {
		t.Errorf("the spliced line reads as code — the escaped text still opens a block:\n%s", spliced)
	}
}

// EscapeDanglingFence's contract survives the block phase byte for byte on
// the shapes the catalogue generator splices: every case of the committed
// suite, re-asserted here so the swap is answerable in one place.
func TestTheEscapeStillLandsBelowTheContainers(t *testing.T) {
	for _, tc := range []struct{ name, src, wantLine string }{
		{"a bullet holds the first delimiter", "What it posts:\n- ```suggestion\nthe replacement line\n```\nAFTER: assignee rules\n", "\\`\\`\\`"},
		{"a blockquote holds the first delimiter", "Note:\n> ```sh\n> run it\n```\nAFTER: assignee rules\n", "\\`\\`\\`"},
		{"six columns of indentation hold no fence at all", "What it posts:\n      ```suggestion\nthe replacement line\n```\nAFTER: assignee rules\n", "\\`\\`\\`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := EscapeDanglingFence(tc.src)
			lines := Lines(out)
			if got := lines[3]; got != tc.wantLine {
				t.Errorf("line 4 of the escaped text is %q, want %q:\n%s", got, tc.wantLine, out)
			}
			if n := strings.Count(out, "\\`"); n != 3 {
				t.Errorf("%d backslash(es) written, want 3:\n%s", n, out)
			}
			var f FenceScanner
			for _, line := range Lines(out) {
				f.Code(line)
			}
			if f.Open() {
				t.Errorf("still open after the repair:\n%s", out)
			}
		})
	}
}
