// Package mdcode answers one question about markdown: which of its bytes are
// code.
//
// A `](target)` inside a fenced block or an inline code span is a form the
// page QUOTES, not a link it makes. A scanner that cannot tell the two apart
// rewrites the quote, reports it as broken, or indexes it as an edge — and a
// documentation repository quotes link forms exactly when it is documenting
// links. The answer lives here once because the alternative is what this
// package was extracted from: every scanner spelling "is this code" its own
// way, and the page that documents the rule breaking the generator.
//
// The rule is CommonMark's, because that is what both readers of these docs
// run. A backtick STRING — a maximal run of backticks — opens a code span,
// and the span ends at the next backtick string of EXACTLY the same length.
// A run with no partner of its own length is literal text, which is what a
// renderer shows for a stray backtick and why Mask must not swallow the rest
// of the line when it meets one.
//
// Not modelled: the CONTENT of the indented (four-space) code block. A
// delimiter indented four or more columns past its container opens no fence
// — the block phase below reads the indented code block, and the fence state
// answers to it — but the block's own lines are still read as prose here:
// every caller reads either a single line of prose or a page whose code is
// fenced, and no link in this repository's corpus sits in an indented
// block.
//
// Four further shapes the block phase reads by APPROXIMATION, named here
// where the old line-by-line fence carried its one greppable limit, so a
// reader can grep for the class and not find it closed:
//
//   - An HTML block is not modelled: a line that opens one (a comment, a
//     tag alone on its line) neither interrupts nor holds a fence, so a
//     delimiter inside the block reads as a fence opener here where a
//     renderer reads HTML through the block's closer.
//   - A setext underline is read where the leaf reads it: under an open
//     paragraph it closes the paragraph and opens nothing (a heading);
//     read as a container's lazy continuation, the run stays paragraph
//     text where a renderer makes it the underline. A `---` keeps the
//     thematic-break reading it also has, which cuts the container.
//   - The GFM table is modelled for its paragraph-state effect alone — a
//     delimiter row makes the rows it caps non-paragraph, so a marker
//     below the rows opens an item paragraph text would refuse. The
//     table's cells are nobody's here.
//   - A tab advances the column to the next multiple of four wherever
//     indentation defines structure; the partial-tab reconstruction
//     CommonMark makes at a container boundary — the spaces its expansion
//     leaves inside the content — is not rebuilt.
//
// Measured against pandoc 3.1.3 (gfm) as the oracle, the approximations
// hold where they claim to on every shape probed; the committed corpus
// carries none of the shapes (the golden would name the file).
//
// A LINE ends at a line feed, a carriage return, or a carriage return and a
// line feed — CommonMark's rule, and the rule the renderer that serves these
// files implements. Splitting on line feeds alone makes a lone carriage
// return invisible to this package while a renderer breaks the line on it,
// and a delimiter run that lands at the start of such a line opens a block
// this package reports as absent. Lines is the one spelling; every walk here
// uses it.
package mdcode

import (
	"regexp"
	"strings"
)

var (
	// spanRe approximates a code span in one regular expression: a run of
	// backticks, the text it opens, and the run that closes it. It cannot
	// require the two runs to be the same LENGTH — that is not a regular
	// language — so Mask uses the scanner below and this stays for the line
	// scanners that TRANSFORM a span's text rather than mask it.
	spanRe = regexp.MustCompile("`+[^`]*`+")
)

// SpanPattern returns the one-regexp approximation of a code span. Prefer
// Spans, which implements the pairing rule; this exists for a caller that
// must replace a span's TEXT — a heading reduced to what GitHub slugs — and
// has always read the approximation.
func SpanPattern() *regexp.Regexp { return spanRe }

// Lines splits md the way a renderer does: at a line feed, at a carriage
// return, or at a carriage return followed by a line feed. The terminators
// are dropped, the lines are not trimmed, and a trailing terminator yields a
// final empty line, as strings.Split does.
//
// It is the entry point for anything that decides markdown structure line by
// line. A caller splitting on "\n" alone reads a lone carriage return as an
// ordinary byte of its line — so a delimiter run that follows one looks
// mid-line to the caller and sits at column 0 for the renderer.
func Lines(md string) []string {
	lines := make([]string, 0, strings.Count(md, "\n")+1)
	for pos := 0; pos <= len(md); {
		line, next := nextLine(md, pos)
		lines = append(lines, line)
		pos = next
	}
	return lines
}

// nextLine returns the line starting at pos and the offset where the next one
// starts — len(md)+1 past the last line, which is what ends a walk. The
// returned line excludes its terminator, so an offset into it is an offset
// into md.
func nextLine(md string, pos int) (line string, next int) {
	rest := md[pos:]
	i := strings.IndexAny(rest, "\n\r")
	if i < 0 {
		return rest, len(md) + 1
	}
	end := pos + i + 1
	if rest[i] == '\r' && i+1 < len(rest) && rest[i+1] == '\n' {
		end++
	}
	return rest[:i], end
}

// Spans returns the byte ranges of one line's inline code spans, in order and
// without overlap: each backtick run is paired with the next run of its own
// length, and an unpaired run is left as the literal text a renderer shows.
//
// The length rule is the whole point. Pairing a run with the next run of ANY
// length masks the text between two literal runs — and a real link written
// between them leaves the graph, which no reader of the page would expect.
func Spans(line string) [][2]int {
	var runs [][2]int
	for i := 0; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		runs = append(runs, [2]int{i, j})
		i = j
	}
	var spans [][2]int
	for a := 0; a < len(runs); a++ {
		width := runs[a][1] - runs[a][0]
		for b := a + 1; b < len(runs); b++ {
			if runs[b][1]-runs[b][0] == width {
				spans = append(spans, [2]int{runs[a][0], runs[b][1]})
				a = b
				break
			}
		}
	}
	return spans
}

// frontMatterEnd matches the delimiter of a YAML front-matter block.
var frontMatterEnd = regexp.MustCompile(`^(---|\.\.\.)\s*$`)

// FrontMatterScanner skips the YAML block a page may open on its FIRST line.
//
// It lives beside FenceScanner because it answers the other half of the same
// question — where a page's prose begins — and because the two consumers that
// disagreed about it published the disagreement: six rows of the committed
// docs map carried `title: Changelog` and `layout: home` as the page's
// opening sentence, because one scanner skipped front matter and the other
// had never heard of it.
//
// The zero value is ready: a document starts before its first line.
type FrontMatterScanner struct {
	seenFirst bool
	open      bool
}

// Skip reports whether the line belongs to the front matter — its delimiters
// included — and advances the scanner.
//
// It lives beside FenceScanner because it answers the other half of the same
// question — where a page's prose begins — and because the two consumers that
// disagreed about it published the disagreement: six rows of the committed
// docs map carried `title: ` and `layout: home` as the page's opening
// sentence, because one scanner skipped front matter and the other had never
// heard of it.
func (f *FrontMatterScanner) Skip(line string) bool {
	if !f.seenFirst {
		f.seenFirst = true
		// Only the FIRST line can open it; a `---` further down is a
		// horizontal rule or a setext underline.
		if strings.HasPrefix(line, "---") && frontMatterEnd.MatchString(line) {
			f.open = true
			return true
		}
		return false
	}
	if !f.open {
		return false
	}
	if frontMatterEnd.MatchString(line) {
		f.open = false
	}
	return true
}

// blockFrame is one container on the stack.
type blockFrame struct {
	kind frameKind
	// col is the frame's content column, counted from the start of the
	// line: where the frame's own blocks begin, and the reach a later line
	// needs to still be inside it.
	col int
	// empty marks a list item that opened with nothing after its marker.
	// Such an item begins with a blank line, and CommonMark lets a list
	// item begin with at most one: the blank line after it ends it, where
	// a blank line ends no other frame.
	empty bool
}

type frameKind uint8

const (
	quoteFrame frameKind = iota
	itemFrame
)

// FenceScanner tracks fenced code blocks across the lines of one document.
//
// It is CommonMark's BLOCK PHASE for the constructs that decide where a
// fence begins and ends: a stack of containers advanced line by line —
// block quotes and list items, each carrying the column its own content
// starts at — over one open leaf, which is a paragraph, a fenced code
// block, an indented code block or a GFM table. Every verdict this package
// reports is the leaf's: a delimiter opens a fence where the leaf phase
// reads one, and a block a container holds ends where its container does.
// The shapes read by approximation are named in the package doc — HTML
// blocks, the setext underline, the GFM table's paragraph-state effect,
// tab reconstruction — not carried silently.
//
// A fence rule has TWO halves — which lines are delimiters, and which
// delimiter closes which fence — and copying one half is worse than copying
// neither: a scanner that recognises `~~~` and blockquoted fences as
// delimiters but closes a block on any delimiter will END a ``` block on the
// `~~~` line inside it, which CommonMark calls content. Measured: that exact
// combination aborted `iterion map gen` on a page whose links were sound.
// So the whole machine lives here and its users hold a scanner, never a
// predicate.
//
// A block a CONTAINER holds ends where its container does: a list item's
// fence is closed by the end of the item, a blockquote's by the end of the
// quote — fenced content is not paragraph continuation text, so a line the
// containers cannot continue is outside it. A scanner blind to that pairs an
// opener the renderer holds inside a container with a delimiter BELOW the
// container, calls the text balanced, and leaves the page the second
// delimiter opens — the phantom fence — to everything after it.
//
// The zero value is ready: a document starts outside a fence.
type FenceScanner struct {
	// frames is the container stack, outermost first: the block quotes and
	// list items the lines so far have opened and not ended. A frame ends
	// at the first line that fails to continue it, and every block it held
	// ends with it.
	frames []blockFrame
	// The one open leaf below the innermost frame. fenceOpen is the fenced
	// code block, with the character and length its closer must match.
	// codeOpen, paraOpen and tableOpen are the indented code block, the
	// paragraph and the GFM table — the three states a delimiter's fate
	// hangs on. A fence's content parses for its closer alone, so the
	// other three are false whenever fenceOpen is true, and at most one
	// leaf is open at a time.
	fenceOpen bool
	fenceChar byte
	fenceLen  int
	codeOpen  bool
	paraOpen  bool
	tableOpen bool
	// pipeAbove and cellsAbove are the header-row candidate a GFM table
	// opens on: whether the line just read carried an unescaped pipe, and
	// how many cells it cuts into. The table is modelled for what it does
	// to FENCE MEMBERSHIP alone — its rows are not paragraph text, so a
	// marker below the rows opens an item where paragraph text would
	// refuse it — and for nothing else.
	pipeAbove  bool
	cellsAbove int
	// Where the open block was opened — the delimiter's offset in its line,
	// and the delimiter — so a caller repairing a dangling opener reads them
	// off the scanner that found it. openedOn says the line just read is
	// that line, which is the only way to see a line that closed the block
	// its container held and opened one of its own: Open is true on both
	// sides of it.
	at       int
	delim    string
	openedOn bool
}

// Open reports whether the scanner is currently inside a fenced block — the
// state left by the lines already fed to Code, before the next one is read.
func (f *FenceScanner) Open() bool { return f.fenceOpen }

// Code reports whether the line is code — a fence delimiter, or a line inside
// a fenced block — and advances the scanner. A delimiter is code too: it is
// not prose, and no user of this type has ever wanted it.
//
// The line is read the way CommonMark reads it, in the phase's own order:
// the blank line that ends no container, then the frames the line still
// continues, then the containers it may open below them, then the leaf.
func (f *FenceScanner) Code(line string) bool {
	f.openedOn = false
	if blankLine(line) {
		// A blank line ends no frame — a quote and a list item both carry
		// on over one. The three line-adjacent leaves end: their content
		// is the lines above, and this line is after them. A fenced block
		// is not line-adjacent — the blank is its content, and the block
		// carries on.
		f.paraOpen, f.codeOpen, f.tableOpen = false, false, false
		f.pipeAbove, f.cellsAbove = false, 0
		// A list item that opened EMPTY is the one frame a blank line
		// ends: CommonMark lets a list item begin with at most one blank
		// line.
		if n := len(f.frames); n > 0 && f.frames[n-1].empty {
			f.cut(n - 1)
		}
		return f.fenceOpen
	}
	// MATCH: continue every frame the line still sits inside, outermost
	// first. A quote continues where at most three columns past its content
	// column a `>` stands; an item where the content reaches its content
	// column. The first frame the line cannot continue ends, with every
	// frame inside it — unless the paragraph it holds continues LAZILY.
	pos, col, depth := 0, 0, 0
	for depth < len(f.frames) {
		fr := f.frames[depth]
		ind, indCol := skipIndent(line, pos, col)
		matched := false
		switch fr.kind {
		case quoteFrame:
			matched = indCol-col <= 3 && ind < len(line) && line[ind] == '>'
			if matched {
				pos, col = ind+1, indCol+1
				if pos < len(line) && line[pos] == ' ' {
					pos, col = pos+1, col+1
				}
			}
		case itemFrame:
			matched = indCol >= fr.col
			if matched {
				pos, col = ind, indCol
				// The item has taken a line: it no longer begins with a
				// blank line, so no blank line ends it any more — the
				// at-most-one-blank rule ends only an item that has
				// received nothing yet.
				f.frames[depth].empty = false
			}
		}
		if matched {
			depth++
			continue
		}
		// The frame ends here — unless the paragraph it holds continues
		// LAZILY: a line that would start no block of its own is that
		// paragraph's next line, and the frames it lives in survive a line
		// that carries none of their markers.
		if f.paraOpen && !f.interrupts(line, ind, indCol) {
			f.paraText(line[ind:])
			return f.fenceOpen
		}
		f.cut(depth)
		break
	}
	// OPEN: below the matched frames the line may open containers of its
	// own — never inside a fenced block, whose content is read for its
	// closer alone.
	if !f.fenceOpen {
		for {
			ind, indCol := skipIndent(line, pos, col)
			if ind >= len(line) || indCol-col > 3 {
				pos, col = ind, indCol
				break
			}
			pos, col = ind, indCol
			if line[pos] == '>' {
				pos, col = pos+1, col+1
				if pos < len(line) && line[pos] == ' ' {
					pos, col = pos+1, col+1
				}
				f.paraOpen = false
				f.frames = append(f.frames, blockFrame{kind: quoteFrame, col: col})
				continue
			}
			// A thematic break carries the same marker a list item would
			// (`- - -`) and opens no item: CommonMark reads the break, and
			// the leaf below reads it as the block it is.
			if thematicBreak(line[pos:]) {
				break
			}
			fr, next, nextCol, ok := listItemFrame(line[pos:], col, f.paraOpen)
			if !ok {
				break
			}
			f.paraOpen = false
			f.frames = append(f.frames, fr)
			pos, col = pos+next, nextCol
		}
	}
	return f.leaf(line, pos, col)
}

// cut ends every frame from depth on. A leaf lives only inside the whole
// chain it was opened under, so every one of them ends with the frames: the
// fenced block a container held, the paragraph, the indented code block, the
// table.
func (f *FenceScanner) cut(depth int) {
	f.frames = f.frames[:depth]
	f.fenceOpen, f.codeOpen, f.paraOpen, f.tableOpen = false, false, false, false
	f.pipeAbove, f.cellsAbove = false, 0
}

// interrupts reports whether the line, read from the position the failed
// frame would have matched at, would start a block of its own — one of the
// starts CommonMark lets interrupt a paragraph, and so not text that can be
// folded into the paragraph above as its lazy continuation. Measured with
// the file's own renderer as the oracle, the starts are: a blockquote, a
// fenced code block, an ATX heading, a thematic break, a GFM table's
// delimiter row, and a list item whose marker may interrupt.
func (f *FenceScanner) interrupts(line string, pos, col int) bool {
	ind, indCol := skipIndent(line, pos, col)
	if ind >= len(line) || indCol-col >= 4 {
		return false // indented code, or the line's end: continuation text either way
	}
	rest := line[ind:]
	if rest[0] == '>' {
		return true
	}
	if run, after := fenceRun(rest); run != "" {
		// The same info-string rule the opener answers to: a backtick run
		// whose info string carries a backtick is paragraph text.
		return run[0] != '`' || !strings.ContainsRune(after, '`')
	}
	if isATXHeading(rest) || thematicBreak(rest) {
		return true
	}
	if isDelimiterRow(rest) && f.pipeAbove && pipeCells(rest) == f.cellsAbove {
		return true
	}
	_, _, _, ok := listItemFrame(rest, indCol, true)
	return ok
}

// paraText records the line as paragraph text — a lazy continuation reads it
// here too — and keeps the header-row candidate it leaves: a GFM table opens
// on the line below only where this line carried an unescaped pipe and cut
// into as many cells as the delimiter row below it.
func (f *FenceScanner) paraText(rest string) {
	f.paraOpen = true
	f.pipeAbove = hasUnescapedPipe(rest)
	f.cellsAbove = pipeCells(rest)
}

// leaf reads the line's own block below the container stack, from the text
// the frames and the ≤3 columns of further indentation have left, and
// returns whether the line is code.
func (f *FenceScanner) leaf(line string, pos, col int) bool {
	base := 0
	if n := len(f.frames); n > 0 {
		base = f.frames[n-1].col
	}
	pos, col = skipIndent(line, pos, col)
	rest := line[pos:]
	rel := col - base
	// A line inside a fenced block is its content, and the one exception is
	// the fence's own closer: a run of the SAME character, at least as long
	// as the one that opened it, with nothing but spaces after it, at most
	// three columns past the frame the fence was opened in. Anything else is
	// content — including the delimiter of a block the page is SHOWING, and
	// any marker a container would have read.
	if f.fenceOpen {
		if rel <= 3 {
			if run, after := fenceRun(rest); run != "" && run[0] == f.fenceChar &&
				len(run) >= f.fenceLen && blankLine(after) {
				f.fenceOpen = false
				f.pipeAbove, f.cellsAbove = false, 0
				return true
			}
		}
		f.pipeAbove, f.cellsAbove = false, 0
		return true
	}
	// The line ends the line-adjacent leaves wherever its own text runs
	// out: paragraph, indented code and table have no lines but the ones
	// above it.
	if blankLine(rest) {
		f.paraOpen, f.codeOpen, f.tableOpen = false, false, false
		f.pipeAbove, f.cellsAbove = false, 0
		return false
	}
	// Four or more columns past the container's content column is an
	// INDENTED CODE BLOCK: no fence delimiter is read there at all, which is
	// the arm that keeps a deep delimiter from opening a block the page
	// never shows. It cannot interrupt a paragraph — under one, the line is
	// that paragraph's continuation text.
	if rel >= 4 {
		if f.paraOpen {
			f.paraText(rest)
			return false
		}
		f.codeOpen = true
		f.paraOpen, f.tableOpen = false, false
		f.pipeAbove, f.cellsAbove = false, 0
		return false
	}
	f.codeOpen = false
	// Block starts, in the order CommonMark reads them.
	if run, after := fenceRun(rest); run != "" {
		if run[0] != '`' || !strings.ContainsRune(after, '`') {
			f.fenceOpen, f.fenceChar, f.fenceLen = true, run[0], len(run)
			f.at, f.delim, f.openedOn = pos, run, true
			f.paraOpen, f.tableOpen, f.pipeAbove, f.cellsAbove = false, false, false, 0
			return true
		}
		// fall through: a backtick fence whose info string carries a
		// backtick is a paragraph, not a fence
	}
	if isATXHeading(rest) || thematicBreak(rest) ||
		(f.paraOpen && isSetextUnderline(rest)) {
		f.paraOpen, f.tableOpen = false, false
		f.pipeAbove, f.cellsAbove = false, 0
		return false
	}
	if f.tableOpen {
		// A table row: cells, not paragraph text, so a marker below the
		// rows opens an item where paragraph text would refuse it.
		f.pipeAbove, f.cellsAbove = hasUnescapedPipe(rest), pipeCells(rest)
		return false
	}
	if isDelimiterRow(rest) && f.pipeAbove && pipeCells(rest) == f.cellsAbove {
		// The delimiter row of a GFM table, under a line that could be its
		// header: the table interrupts the paragraph above it.
		f.tableOpen = true
		f.paraOpen = false
		return false
	}
	f.paraText(rest)
	return false
}

// skipIndent returns the text past the spaces and tabs that follow pos,
// carrying col along: a tab takes the cursor to the next multiple of four
// columns, wherever indentation defines block structure.
func skipIndent(s string, pos, col int) (int, int) {
	for pos < len(s) {
		switch s[pos] {
		case ' ':
			pos++
			col++
		case '\t':
			col = advance(col, '\t')
			pos++
		default:
			return pos, col
		}
	}
	return pos, col
}

// blankLine reports whether the text is spaces and tabs only — CommonMark's
// blank line, the one that ends no container.
func blankLine(s string) bool {
	return strings.TrimLeft(s, " \t") == ""
}

// fenceRun returns the fenced-block delimiter the text opens with — a run of
// at least three backticks or tildes — and what follows it, or ("", s) when
// the text starts with none.
func fenceRun(s string) (string, string) {
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return "", s
	}
	i := 1
	for i < len(s) && s[i] == s[0] {
		i++
	}
	if i < 3 {
		return "", s
	}
	return s[:i], s[i:]
}

// listItemFrame reads a list item opening at the text's first byte and
// returns the frame it opens — kind and content column — beside the
// position the item's own content starts at, or ok=false where no item
// opens: a marker glued to a word, an ordered marker outside
// [0-9]{1,9}[.)], or a marker that cannot interrupt the open paragraph (an
// empty item, or an ordered one not starting at 1).
//
// CommonMark puts the content column one space past the marker, or at the
// first non-space byte when one to four spaces follow it; five or more make
// the item's first block an indented code block starting one column past
// the marker, and the column the frame carries is that one column past. A
// marker with nothing but spaces after it opens an empty item, also one
// column past.
//
// Reading an item the page does not have is the one error this must not
// make: the repair then lands on the author's CLOSER and leaves the real
// opener live, a phantom fence of the generator's own making. Measured on
// both renderers: under a prose line, `2) a step` is paragraph text and a
// lone `-` is a setext underline, where after a blank line both open a
// list.
func listItemFrame(s string, col int, paraOpen bool) (fr blockFrame, next, nextCol int, ok bool) {
	marker := 0
	ordered := false
	switch s[0] {
	case '-', '+', '*':
		marker = 1
	default:
		for marker < len(s) && marker < 9 && s[marker] >= '0' && s[marker] <= '9' {
			marker++
		}
		if marker == 0 || marker >= len(s) || (s[marker] != '.' && s[marker] != ')') {
			return blockFrame{}, 0, 0, false
		}
		marker++
		ordered = true
	}
	end := col + marker
	pos, after := skipIndent(s, marker, end)
	switch {
	case pos >= len(s):
		// Nothing after the marker: an empty item.
		if paraOpen {
			return blockFrame{}, 0, 0, false
		}
		return blockFrame{kind: itemFrame, col: end + 1, empty: true}, pos, after, true
	case after == end:
		return blockFrame{}, 0, 0, false // the marker takes a space or the line's end; `-x` is a word
	case paraOpen && ordered && !startsAtOne(s[:marker-1]):
		return blockFrame{}, 0, 0, false
	}
	content := after
	if after-end > 4 {
		content = end + 1
	}
	return blockFrame{kind: itemFrame, col: content}, pos, after, true
}

// startsAtOne reports whether an ordered marker's digits read as the number
// 1 — the only start CommonMark lets interrupt a paragraph.
func startsAtOne(digits string) bool {
	return strings.TrimLeft(digits, "0") == "1"
}

// isATXHeading reports whether the text opens an ATX heading: one to six
// `#`, then the line's end or a space.
func isATXHeading(s string) bool {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	return n > 0 && n <= 6 && (n == len(s) || s[n] == ' ' || s[n] == '\t')
}

// isSetextUnderline reports whether the text is a setext heading's
// underline: a run of `=` or a run of `-`, with nothing but trailing
// spaces or tabs. Under an open paragraph it closes it — the heading it
// makes opens nothing, and the paragraph's marker rules stop applying
// below it; under no paragraph the run is paragraph text, which is why
// only the leaf reads it. A `---` is at the same time the thematic break
// it also is, and reads as one whether or not a paragraph stands above.
func isSetextUnderline(s string) bool {
	t := strings.TrimRight(s, " \t")
	if t == "" || (t[0] != '=' && t[0] != '-') {
		return false
	}
	for i := 1; i < len(t); i++ {
		if t[i] != t[0] {
			return false
		}
	}
	return true
}

// tableDelimRow matches the delimiter row of a GFM table: cells of hyphens
// and colons cut by pipes, the outer pipes optional.
var tableDelimRow = regexp.MustCompile(`^\|?(?:[ \t]*:?-+:?[ \t]*\|)*[ \t]*:?-+:?[ \t]*\|?$`)

// isDelimiterRow reports whether the text is a GFM table's delimiter row.
func isDelimiterRow(s string) bool { return tableDelimRow.MatchString(s) }

// hasUnescapedPipe reports whether the text carries a pipe no backslash
// escapes — the mark a GFM table's header row carries.
func hasUnescapedPipe(s string) bool {
	slashes := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			slashes++
			continue
		}
		if s[i] == '|' && slashes%2 == 0 {
			return true
		}
		slashes = 0
	}
	return false
}

// pipeCells counts the cells the text cuts into on its unescaped pipes, with
// an outer pipe on either end counting for nothing — the measure a delimiter
// row must match against the line above it.
func pipeCells(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	pipes, slashes, first, last := 0, 0, -1, -1
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			slashes++
			continue
		}
		if s[i] == '|' && slashes%2 == 0 {
			if first < 0 {
				first = i
			}
			last = i
			pipes++
		}
		slashes = 0
	}
	cells := pipes + 1
	if first == 0 {
		cells--
	}
	if last == len(s)-1 {
		cells--
	}
	return cells
}

// thematicBreak reports whether the line is a run of three or more of the
// same `-`, `*` or `_`, spaces or tabs apart — and nothing else: a `>`
// cannot stand inside a break, so `- - > -` is a list in a list in a quote
// and only the text BELOW the container markers can be a break at all. The
// frames hand it that text — a leading `>` never reaches here — which is
// why the run it reads is checked whole.
func thematicBreak(line string) bool {
	trimmed := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, line)
	if len(trimmed) < 3 {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != trimmed[0] {
			return false
		}
	}
	return trimmed[0] == '-' || trimmed[0] == '*' || trimmed[0] == '_'
}

// advance returns the column a prefix byte moves the cursor to: a tab goes to
// the next multiple of four, anything else one column on.
func advance(col int, b byte) int {
	if b == '\t' {
		return col + 4 - col%4
	}
	return col + 1
}

// Mask returns md with every byte of code replaced by a space: the body of a
// fenced block, its delimiter lines, and every inline code span.
//
// Length and line structure are preserved, so an offset into the result is
// the same offset in the input. That is the whole interface: a caller keeps
// its own grammar for what it is looking for, runs it over the mask, and
// reads the bytes it found out of the original. Nothing here needs to know
// what a link is.
func Mask(md string) string {
	out := []byte(md)
	blank := func(from, to int) {
		for i := from; i < to; i++ {
			out[i] = ' '
		}
	}
	var fence FenceScanner
	for pos := 0; pos <= len(md); {
		line, next := nextLine(md, pos)
		if fence.Code(line) {
			blank(pos, pos+len(line))
		} else {
			for _, r := range Spans(line) {
				blank(pos+r[0], pos+r[1])
			}
		}
		pos = next
	}
	return string(out)
}

// ReplaceOutsideCode rewrites every match of re that falls entirely outside
// code, and reproduces every other byte of md as written.
//
// It is Mask's offset contract plus the one rule a caller running a grammar
// over the mask cannot skip: the mask can WIDEN a window. A `)` an inline
// span carried is blanked, so a link destination runs past the parenthesis
// that ends it and swallows the span — and handing that window to rewrite
// re-emits a replacement in place of bytes the document wrote, which is text
// loss, not a rewrite. Only a window the mask agrees is prose end to end is
// rewritten.
func ReplaceOutsideCode(md string, re *regexp.Regexp, rewrite func(match string) string) string {
	masked := Mask(md)
	var out strings.Builder
	end := 0
	for _, loc := range re.FindAllStringIndex(masked, -1) {
		out.WriteString(md[end:loc[0]])
		if window := md[loc[0]:loc[1]]; masked[loc[0]:loc[1]] == window {
			out.WriteString(rewrite(window))
		} else {
			out.WriteString(window)
		}
		end = loc[1]
	}
	out.WriteString(md[end:])
	return out.String()
}

// MaskDocument masks a whole page: its YAML front matter as well as its code.
//
// Mask alone answers for code, which is what a caller holding one LINE wants.
// A caller holding a whole page wants both, because a `](target)` in front
// matter is configuration, not a link the page makes.
func MaskDocument(md string) string {
	out := []byte(Mask(md))
	var front FrontMatterScanner
	for pos := 0; pos <= len(md); {
		line, next := nextLine(md, pos)
		if front.Skip(line) {
			for i := pos; i < pos+len(line); i++ {
				out[i] = ' '
			}
		}
		pos = next
	}
	return string(out)
}

// EscapeDanglingFence returns md with the fenced-block opener it leaves
// UNCLOSED backslash-escaped, so md can be embedded in a larger document
// without opening a block that swallows every line spliced after it.
//
// It is what a caller that EMBEDS free text owes the document around it. A
// paragraph word-wrapped onto a line that happens to begin with three
// backticks is a wrap accident, and the renderer that reads it as a block
// opener takes the rest of the page with it — hundreds of lines of a
// generated catalogue, which every scanner of this package then reads as
// code.
//
// Only a DANGLING opener is escaped. A fence the text opens AND closes is a
// block its author wrote on purpose, and rewriting deliberate markup is the
// defect this package exists to prevent; the embedded text keeps it whole.
//
// EVERY character of the delimiter run is escaped, not one backslash before
// it. CommonMark consumes one backtick per backslash, so a single backslash
// leaves a run of n-1 — still a code-span delimiter, and it pairs with the
// next run of that width in the text: one backslash before three backticks,
// with a two-backtick span further down, renders everything between the two
// as code and eats the author's own span. Escaped in full, the run is
// literal text for a renderer, and the bytes Mask blanks are the escape's
// own: the width-1 runs it leaves pair among themselves, and the odd one
// out finds no partner, because a line carrying another backtick is no
// backtick opener at all (the info-string rule) and a tilde opener leaves
// no backtick behind.
//
// Escaping one opener can promote a line the open block held as CONTENT
// into an opener of its own, so the scan repeats. The repeat carries the
// container state the escape point left open and re-reads from the opener
// line itself, now escaped: a delimiter that sat invisible because the
// dangling block swallowed it — four columns in, or inside a list item —
// opens a block of its own once the block above it is gone, and a rescan
// that started past the escape with a fresh scanner would read it as the
// indented text it no longer is. Escaped lines open nothing — the opener
// rule admits only spaces, blockquote and list markers before the run — so
// every escape moves the resumption strictly forward and the walk
// terminates.
//
// Cost, since the text is a third party's and a caller may escape it at
// every start: one pass over the lines per DANGLING opener — text that
// closes what it opens is read once, and a wrap accident costs one more
// read of everything below its opener. Bounding the passes is not the
// answer — stopping early would hand the document the very dangling fence
// this exists to remove.
//
// The text comes back with its line endings normalised to line feeds. The
// promise is about a RENDERER's reading, and the renderer that serves these
// files breaks a line on a lone carriage return as readily as on a line feed
// (measured): a text keeping one would carry a line break this package cannot
// see, and the delimiter run after it would sit at column 0 for every reader
// but this one. The normalisation is also what makes the two readers of these
// files agree — pandoc 3.1.3 DELETES a lone carriage return, joining the
// lines, where GitHub breaks on it — so the line feed is the one ending both
// read as the break the author wrote, and the result is a text whose lines
// are the same for the embedder, for Mask and for the page.
//
// The repair lands where the BLOCK PHASE says the dangling opener is: the
// FenceScanner behind this function reads the container stack — block
// quotes, list items with their content columns, the indented code block,
// the interruption and lazy-continuation rules — so a delimiter the page
// reads as paragraph text is never the repair's target, and the column-0
// delimiter a renderer really opens is never missed. Measured against
// pandoc 3.1.3 (gfm) as the oracle over the 14 580-shape adversarial corpus
// of the catalogue-fence review, run through the real generator: every shape
// the generator used to leak is now repaired, and none of the legitimate
// balanced shapes is rewritten — the numbers and the shapes live in
// TestBlockPhaseClosesTheResidualClass and in the corpus sweep of the
// ticket's review.
func EscapeDanglingFence(md string) string {
	lines := Lines(md)
	var f FenceScanner
	// at is the last opener still live, with the delimiter read off the
	// scanner, and before it the scanner state that line started from —
	// the state the re-read resumes from once the line is escaped.
	at, marker, delim := -1, -1, ""
	var before, atBefore FenceScanner
	for i := 0; i < len(lines); i++ {
		before = f
		f.Code(lines[i])
		switch {
		case f.openedOn:
			at, marker, delim = i, f.at, f.delim
			atBefore = before
		case !f.Open():
			at, marker, delim = -1, -1, ""
		}
		if i+1 < len(lines) || !f.fenceOpen || at < 0 {
			continue
		}
		lines[at] = lines[at][:marker] +
			strings.Repeat(`\`+delim[:1], len(delim)) +
			lines[at][marker+len(delim):]
		f = atBefore
		// the escaped line is re-read — it opens nothing now, the text
		// below it still may; i-1 because the loop's own increment follows
		i = at - 1
		at, marker, delim = -1, -1, ""
	}
	return strings.Join(lines, "\n")
}

// Inline returns s as ONE line, so a caller can splice it inside a line of a
// larger document — a table cell, a heading, a list item — and have that
// line end where the caller ends it.
//
// A value carrying a line break would end its line early and put the rest of
// itself at column 0, where a delimiter run opens a fenced block over
// everything below (and where a table row loses its remaining cells). Text
// with no line break is returned byte for byte: an embedder owes the
// document one line, not a reformatting of the author's words.
func Inline(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.Join(strings.Fields(s), " ")
}

// Cell returns s as the content of ONE cell of a markdown table: Inline's
// single line, with every UNESCAPED pipe escaped.
//
// A row is cut into cells on its unescaped pipes before any of its text is
// read — inside a code span as well as in prose — so a pipe a value carries
// does not print: it ENDS the cell, moves every value after it one column
// left, and the row's last cell falls off the table. Measured on both
// renderers of these files (GitHub's /markdown API and pandoc 3.1.3 gfm):
// `| Revi | the reviewer | ` + "`rev`" + ` | verify |` in a three-column
// table shows "the reviewer" under the second heading and drops "verify"
// altogether; the escaped form shows the pipe, in a span as well as in
// prose.
//
// A pipe the TEXT already escapes is left alone, which is the rule the row's
// own reader applies: a pipe is a separator when the run of backslashes
// before it is even. Escaping it again writes `\\|` — an escaped backslash
// followed by a LIVE separator — and that breaks the very cell this
// repairs: measured on pandoc 3.1.3 gfm, `Revi \| the reviewer` escaped
// twice gains a column and drops the row's last cell, where the author's own
// `\|` keeps its three and renders the pipe.
//
// The repair is lossless — the cell renders what the text means, a pipe for
// `\|` in prose and in a code span alike — so a value legitimately carrying
// a pipe needs no author-side rule, unlike a dangling fence, whose repair
// writes backslashes into the author's words. A value with no pipe comes
// back byte for byte.
func Cell(s string) string {
	line := Inline(s)
	var b strings.Builder
	b.Grow(len(line))
	slashes := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '|' && slashes%2 == 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
		if c == '\\' {
			slashes++
		} else {
			slashes = 0
		}
	}
	return b.String()
}

// danglingFenceOpener returns the index of the line opening the fenced block
// the lines never close, the byte offset of that line's delimiter and the
// delimiter itself, or (-1, -1, "") when every block opened is closed.
//
// A CommonMark fence does not nest — at most one block is open at a time —
// so there is at most one dangling opener, and it is the last one opened.
//
// This fresh full scan is the contract the repair answers to; the walk
// inside EscapeDanglingFence must find the same opener, and the witnesses
// re-read the repaired text through it to prove the repair complete.
func danglingFenceOpener(lines []string) (line, marker int, delim string) {
	line, marker = -1, -1
	var fence FenceScanner
	for i, text := range lines {
		fence.Code(text)
		switch {
		case fence.openedOn:
			// Read off the scanner rather than re-derived: a line can close
			// the block its container held and open one of its own, and
			// Open is true on both sides of such a line. Comparing Open
			// before and after would see no change there and go on blaming
			// the opener the container already closed — one escape too
			// many, written into the author's words, and the real opener
			// left live.
			line, marker, delim = i, fence.at, fence.delim
		case !fence.Open():
			line, marker, delim = -1, -1, ""
		}
	}
	if !fence.Open() {
		return -1, -1, ""
	}
	return line, marker, delim
}

// CloseDanglingSpan repairs a string that opens a code span it does not
// close, by appending the backtick run that closes it.
//
// It is what a caller that TRUNCATES prose owes every scanner downstream. A
// cut lands where a byte bound falls, and one landing between a span's two
// runs leaves half a span: the renderers then show a stray backtick, and
// every reader of this package sees the quoted text as prose — so a link
// form the page only quoted is read as a link, refused, and the generator
// that was cutting the sentence fails on a page whose links are sound.
//
// Closing rather than cutting, because cutting loses text: a cut at the first
// backtick of a 60-byte ADR status leaves an EMPTY cell, and a cut mid-cell
// drops the very name the sentence was quoting.
//
// The caller owes a SINGLE line: Spans pairs runs across a line ending while
// Mask reasons per line, so a repair spanning one would satisfy Spans and
// still show backticks on the page. Both callers collapse first, with Lines
// — every ending a renderer breaks on, since a caller collapsing line feeds
// alone keeps a lone carriage return and hands this two lines.
//
// The dangling run is the first one NO span covers, which is not the same as
// the first one after the last span: a cut inside a `…` form followed by a
// closed “…“ form leaves the dangling run BEFORE a pair, and looking only
// past the last pair reported nothing to repair. A page that quotes two
// backtick widths is exactly the page that documents this rule.
func CloseDanglingSpan(s string) string {
	spans := Spans(s)
	covered := func(pos int) bool {
		for _, r := range spans {
			if pos >= r[0] && pos < r[1] {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		if covered(i) {
			i = j
			continue
		}
		// A run with nothing after it opened nothing worth keeping.
		if strings.TrimSpace(s[j:]) == "" {
			return s[:i]
		}
		// A separator when the text already ends in backticks: appended
		// directly, the closing run would MERGE with the trailing one into
		// a single wider run, which closes nothing.
		sep := ""
		if s[len(s)-1] == '`' {
			sep = " "
		}
		return s + sep + strings.Repeat("`", j-i)
	}
	return s
}
