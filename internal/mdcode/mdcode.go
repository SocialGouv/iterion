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
// Not modelled: the indented (four-space) code block. Every caller reads
// either a single line of prose or a page whose code is fenced, and no link
// in this repository's corpus sits in an indented block.
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
	// fenceRe matches a line that OPENS a fenced code block, including one a
	// container has pushed off column 0: a blockquote, a list item, or a
	// blockquote carrying a list. A delimiter rule that stopped at column 0
	// could not see such an opener at all, and everything below a `>   ```md`
	// or a `- ``` ` line read as prose.
	//
	// Indentation is not bounded here: `mdcode` does not model the indented
	// code block that four columns would otherwise mean.
	//
	// STATED LIMIT: accepting a list marker with no block parser behind it
	// means a fence can be opened where CommonMark starts no container at all
	// — `2) ``` ` interrupting a paragraph, where only `1.` may — and kept
	// open past the container's end. Greppable here rather than silent.
	fenceRe = regexp.MustCompile("^\\s*(?:>\\s?)*\\s*(?:(?:[-*+]|[0-9]{1,9}[.)])[ \\t]+)?(`{3,}|~{3,})")
	// fenceCloseRe is the same rule WITHOUT the list marker. A marker opens a
	// new list item whose first block is the fence, so it can only introduce
	// an OPENER; a closing fence takes nothing but spaces and quote markers
	// before it. Reading `+ ``` ` inside an open block as its closer ends the
	// block on a line CommonMark keeps inside it — and that shape is written
	// in this org's own tree, a ```markdown block showing a ```diff block.
	fenceCloseRe = regexp.MustCompile("^\\s*(?:>\\s?)*\\s*(`{3,}|~{3,})")
	// itemRe matches a line that OPENS a list item: the blockquote and
	// indentation prefix, the bullet or ordered marker, and the whitespace
	// run after it (empty at the end of the line). What the three groups
	// measure is the item's content column — see itemOpener.
	itemRe = regexp.MustCompile(`^([ \t]*(?:>[ \t]?)*[ \t]*)((?:[-*+]|[0-9]{1,9}[.)]))([ \t]*)`)
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

// fenceMarker returns the fenced-block delimiter a line carries, or "".
//
// A backtick fence's info string may not itself contain a backtick: "```go
// `x`" is a PARAGRAPH, not a fence. Reading one as an opener costs the whole
// rest of the page, because no later line closes it.
//
// Unexported, and taking its pattern as a parameter rather than reading a
// package var: WHICH pattern applies depends on whether a block is open, and
// an exported helper answering with only one of them would invite a caller to
// decide a close with the opener's rule — the defect that cost a round.
func fenceMarker(line string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	marker := m[1]
	if marker[0] == '`' && strings.ContainsRune(line[strings.Index(line, marker)+len(marker):], '`') {
		return ""
	}
	return marker
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

// FenceScanner tracks fenced code blocks across the lines of one document.
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
// quote — fenced content is not paragraph continuation text, so a line less
// indented than the container's content column is outside it. A scanner
// blind to that pairs an opener the renderer holds inside a container with a
// delimiter BELOW the container, calls the text balanced, and leaves the
// page the second delimiter opens — the phantom fence — to everything after
// it.
//
// The zero value is ready: a document starts outside a fence.
type FenceScanner struct {
	open  bool
	char  byte
	n     int
	quote int
	// hold is the content column of the container holding the open block: a
	// non-blank line indented less than it is outside the container, which
	// closes the block. A LIST MARKER on the opener's own line gives it
	// exactly; a list item OPEN from an earlier line gives it too (item
	// below); an opener four or more columns in bounds it, since a fence
	// takes at most three columns past its container; it is 0 for an opener
	// that needs no container, where the rule is the delimiters' alone.
	hold int
	// item is the content column of the innermost list item open at the line
	// just read, 0 for none, and itemQuote the blockquote depth it was opened
	// at. A content column is at least 2 — a marker and its space — so 0 is
	// no item, and the zero value is a document inside no container.
	//
	// It is kept because a fence's container is usually NOT on the fence's
	// own line: an author writes the example indented under the bullet that
	// introduces it, and the item is what the renderer closes the block
	// with. Tracked across lines, not read off the delimiter, and only while
	// no block is open: a line inside a fenced block is content and opens no
	// container.
	item      int
	itemQuote int
	// paraAbove says a paragraph may be open on the line above the one being
	// read: a non-blank line outside a fenced block leaves one, where a
	// blank line, a fence delimiter and the start of the text leave none. It
	// is what tells itemOpener whether a marker CommonMark forbids from
	// interrupting a paragraph starts a list item where it sits.
	paraAbove bool
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
func (f *FenceScanner) Open() bool { return f.open }

// Code reports whether the line is code — a fence delimiter, or a line inside
// a fenced block — and advances the scanner. A delimiter is code too: it is
// not prose, and no user of this type has ever wanted it.
func (f *FenceScanner) Code(line string) bool {
	f.openedOn = false
	// The container holding the block ends before this line, so the block
	// ends with it — and the line is read again from the closed state, where
	// a delimiter on it OPENS a block instead of closing one no renderer
	// kept open.
	if f.open && f.containerEnded(line) {
		f.open = false
	}
	// Outside a block the line can open, continue or end a list item, which
	// is the container the next opener will be held by.
	if !f.open {
		f.trackItem(line)
	}
	// An open block is closed by a bare delimiter only: a list marker starts
	// a new item, which is content inside the block, never its closer.
	re := fenceRe
	if f.open {
		re = fenceCloseRe
	}
	marker := fenceMarker(line, re)
	if marker == "" {
		return f.open
	}
	// A delimiter behind a LIST MARKER opens a block only where that marker
	// opens an ITEM. `2) ``` ` under a prose line opens neither: both
	// renderers read the whole line as paragraph text (only a list starting
	// at 1 may interrupt a paragraph), and a reading that took it for an
	// opener would pair it with the page's REAL opener below and call the
	// text balanced. Which is fenceRe's own stated limit, answered where the
	// line's context is known.
	if !f.open && fenceMarker(line, fenceCloseRe) != marker {
		if _, _, ok := itemOpener(line, f.paraAbove); !ok {
			return false // paragraph text, as trackItem has just recorded
		}
	}
	// A delimiter line is no paragraph text, and neither is the block it
	// opens: the line below it starts a block of its own.
	f.paraAbove = false
	quote, indent := fencePrefix(line, marker)
	if !f.open {
		f.open, f.char, f.n, f.quote = true, marker[0], len(marker), quote
		f.hold = f.containerColumn(line, marker, indent)
		f.at, f.delim, f.openedOn = strings.Index(line, marker), marker, true
		return true
	}
	// A fence closes only on a run of the SAME character, at least as long as
	// the one that opened it, with nothing after it, at the SAME blockquote
	// depth and no more than three columns past its CONTAINER's content
	// column — CommonMark's own measure. Anything else is content, and the
	// delimiter of a block a page is SHOWING is content by exactly this
	// rule: `> ``` ` closing a block opened at depth 0 is a quote of a
	// closer, not a closer.
	rest := strings.TrimSpace(line[strings.Index(line, marker)+len(marker):])
	if quote == f.quote && indent <= f.hold+3 &&
		marker[0] == f.char && len(marker) >= f.n && rest == "" {
		f.open = false
	}
	return true
}

// containerEnded reports whether the line sits outside the container that
// holds the open block: fewer blockquote markers than the opener carried, or
// content starting before the container's content column.
//
// A blank line ends no container — a list item and a blockquote both carry on
// over one — and a line further in is content: a fenced block takes
// everything until its closer, so the only thing that can end it early is
// losing the container it was opened in. A blockquote marker BEYOND the
// opener's depth is content where it is indented into the container and
// outside it where it is not, which is why depth alone cannot answer: a
// `> ``` ` at column 0 is content inside a column-0 block and the end of a
// list item whose content column is two.
func (f *FenceScanner) containerEnded(line string) bool {
	quote, indent, blank := lineColumns(line, f.quote)
	if blank {
		return false
	}
	if quote < f.quote {
		return true
	}
	return indent < f.hold
}

// containerColumn returns the content column of the container holding a fence
// opened on this line, which is where the block ends if the container does.
//
// A LIST MARKER on the opener's own line IS that container, and the column
// its delimiter sits at is the item's content column. Next, the list item
// tracked from the lines ABOVE: an opener indented into an item open since an
// earlier line is held by that item, which is how an author writes a fenced
// example — indented under the bullet that introduces it — and the closing
// delimiter at the bullet's own column then ends the item and the block with
// it, as both renderers do.
//
// Otherwise there is no container to read — but CommonMark gives a fence at
// most three columns past its container's content column, so an opener four
// or more columns in is held by one that indents at least that far less. An
// opener within three columns of its line's start, inside no item, needs no
// container at all, and 0 leaves the rule to the delimiters.
func (f *FenceScanner) containerColumn(line, marker string, indent int) int {
	if fenceMarker(line, fenceCloseRe) != marker {
		return indent
	}
	if indent > 3 {
		return indent - 3
	}
	if f.item > 0 {
		return f.item
	}
	return 0
}

// trackItem advances the innermost open list item over one line read outside
// a fenced block: the item ends where a non-blank line is indented less than
// its content column or carries fewer blockquote markers, and a line opening
// an item of its own becomes the innermost one.
//
// A blank line ends no item, as it ends no other container. Only the
// INNERMOST item is kept: it is the one whose end closes a block opened
// inside it, and an enclosing item's column is lower, so an opener held by
// the outer one is held at least as long by this reading.
//
// The blockquote depth is part of the item, and the reset is what keeps the
// item's column in the SAME frame as a later opener's indentation: both are
// measured past the innermost quote marker, so an item surviving to a line
// is an item at that line's depth. containerColumn therefore reads the
// column with no depth test of its own.
func (f *FenceScanner) trackItem(line string) {
	quote, indent, blank := lineColumns(line, f.itemQuote)
	if blank {
		f.paraAbove = false
		return
	}
	if quote < f.itemQuote || indent < f.item {
		f.item, f.itemQuote = 0, 0
	}
	if q, content, ok := itemOpener(line, f.paraAbove); ok {
		f.item, f.itemQuote = content, q
	}
	f.paraAbove = true
}

// itemOpener reads a line that OPENS a list item and returns the blockquote
// depth it opens at and the item's content column — where the item's own
// blocks start, and so the column a later line must reach to still be inside
// it.
//
// CommonMark puts that column one space past the marker, or at the first
// non-space byte when one to four spaces follow it; five or more make the
// item's first block an indented code block starting one column past the
// marker. A marker with nothing but spaces after it opens an empty item,
// also one column past.
//
// A thematic break is not an item: `* * *` and `- - -` carry a marker and a
// space, and a reading that opened an item there would hold a later fence at
// a column no container has.
//
// ok only where the marker CERTAINLY opens one. A bullet and an ordered
// marker starting at 1, both with content, do wherever they sit: CommonMark
// lets exactly those interrupt a paragraph. Any other marker — an ordered
// one starting elsewhere, or one with nothing after it — opens a list only
// where no paragraph is open above it, which paraAbove answers.
//
// Reading an item the page does not have is the one error this must not
// make: the repair then lands on the author's CLOSER and leaves the real
// opener live, a phantom fence of the generator's own making. Measured on
// both renderers: under a prose line, `2) a step` is paragraph text and a
// lone `-` is a setext heading, where after a blank line both open a list.
func itemOpener(line string, paraAbove bool) (quote, content int, ok bool) {
	m := itemRe.FindStringSubmatchIndex(line)
	if m == nil || thematicBreak(line) {
		return 0, 0, false
	}
	if mk := line[m[4]:m[5]]; paraAbove &&
		(m[7] == len(line) || (mk[0] >= '0' && mk[0] <= '9' && strings.TrimLeft(mk[:len(mk)-1], "0") != "1")) {
		return 0, 0, false
	}
	quote, indent := prefixColumns(line[m[2]:m[3]])
	col := indent + m[5] - m[4]
	gap := 0
	for i := m[6]; i < m[7]; i++ {
		gap = advance(col+gap, line[i]) - col
	}
	switch {
	case m[7] == len(line):
		// Nothing but the marker and spaces: an empty item.
		return quote, col + 1, true
	case gap == 0:
		// A marker takes a space or the end of the line; `-x` is a word.
		return 0, 0, false
	case gap <= 4:
		return quote, col + gap, true
	default:
		return quote, col + 1, true
	}
}

// thematicBreak reports whether the line is a run of three or more of the
// same `-`, `*` or `_`, spaces apart — the form a list marker and a space
// also match.
func thematicBreak(line string) bool {
	trimmed := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '>' {
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

// fencePrefix reads the blockquote depth and the indentation out of what
// fenceMarker's pattern consumed before the marker.
//
// Everything before the marker counts as indentation, a LIST MARKER
// included: the marker is the container, and the column its delimiter sits
// at is the content column of the item it opens.
func fencePrefix(line, marker string) (quote, indent int) {
	return prefixColumns(line[:strings.Index(line, marker)])
}

// lineColumns reads the same two numbers for a line carrying no delimiter —
// how many of the blockquote markers it opens with (never more than depth,
// which is the depth the answer is wanted in) and the column its content
// starts at — and reports a line that is nothing but that prefix as blank.
//
// A LIST MARKER is content here, where fencePrefix counts it as indentation:
// a `- ` on a line of its own starts an item at the column the marker sits
// at, which is what decides whether the line is inside the container that
// started one above it. So is a blockquote marker past depth.
func lineColumns(line string, depth int) (quote, indent int, blank bool) {
	col, base, i := 0, 0, 0
	for i < len(line) {
		switch {
		case line[i] == ' ' || line[i] == '\t':
			col = advance(col, line[i])
			i++
		case line[i] == '>' && quote < depth:
			quote++
			col++
			i++
			// The marker eats one space of what follows it.
			if i < len(line) && line[i] == ' ' {
				col++
				i++
			}
			base = col
		default:
			return quote, col - base, false
		}
	}
	return quote, col - base, true
}

// prefixColumns walks what a delimiter pattern consumed before its marker —
// spaces, tabs, blockquote markers, a list marker — and returns the
// blockquote depth and the columns between the innermost marker's content
// column and the end of the prefix.
//
// Indentation is a number of COLUMNS, not of bytes: wherever indentation
// defines block structure, CommonMark gives a tab the width that takes it to
// the next multiple of four, and both renderers of these files do. Counted in
// bytes a tab measures one, so a tab-indented delimiter came out three columns
// INSIDE the window that closes a block — it closed a fence that stays open
// for a renderer, which is the phantom fence this package exists to prevent.
//
// The columns run from the START of the line, and what they are measured
// AGAINST is the innermost blockquote's content column — marker and its one
// space. The two together are what makes `> \t``` ` land two columns past the
// quote's content column while `\t``` ` lands four past the page's, which is
// the difference between closing a block and being its content (measured on
// both renderers).
func prefixColumns(pre string) (quote, indent int) {
	col, base := 0, 0
	for i := 0; i < len(pre); i++ {
		if pre[i] == '>' {
			quote++
			col++
			if i+1 < len(pre) && pre[i+1] == ' ' {
				col++
				i++
			}
			base = col
			continue
		}
		col = advance(col, pre[i])
	}
	return quote, col - base
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
// backtick opener at all (fenceMarker's info-string rule) and a tilde
// opener leaves no backtick behind.
//
// Escaping one opener can promote a line the open block held as CONTENT
// into an opener of its own, so the scan repeats — resuming at the line
// AFTER the escape, because escaping a dangling opener leaves every line up
// to it balanced: a dangling opener is the last one opened, and an escaped
// line opens nothing (the opener rule admits only spaces, blockquote and
// list markers before the run).
//
// Cost, since the text is a third party's and a caller may escape it at
// every start: one pass over the lines per DANGLING opener — text that
// closes what it opens is read twice, and a wrap accident costs one more
// read each. A text whose every line opens a block it never closes is
// therefore quadratic in its lines; measured at 20 000 such lines, 35 s.
// Bounding the passes is not the answer — stopping early would hand the
// document the very dangling fence this exists to remove.
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
// CONSERVATIVE, deliberately: an opener a CONTAINER holds is neutralised
// too, though both renderers close such a block with its container and it
// swallows nothing of a document spliced after it at column 0 — measured on
// GitHub's /markdown API and pandoc 3.1.3 for a list item, a blockquote and
// a six-column delimiter, with the catalogue's own continuation under them.
// A caller is free to INDENT what it splices (renderCatalogCard does, under
// "Use when") and this function is not told, so the choice is to spend a
// backslash on a sound text rather than leak a fence into an indented one.
//
// STATED LIMIT: "leaves no block open" is FenceScanner's reading, and that
// reading has no BLOCK PHASE. Three questions CommonMark answers there
// decide where a fence begins, and each one has a shape that leaks here:
//
//   - are four columns an indented code block? A delimiter FOUR or more
//     columns in carries no marker to read a container off, so
//     containerColumn bounds one three columns less. A renderer opens no
//     fence there at all, which makes the later delimiter two or three
//     columns in — the one this reading pairs with it — the page's real
//     opener, and everything below it code.
//   - does this marker open a list ITEM where it sits? fenceRe accepts
//     `2) ``` ` as an opener where only a list starting at 1 may interrupt
//     a paragraph, and the page reads that line as paragraph text. The
//     mirror image is refused rather than guessed: itemOpener tracks an
//     item only where the marker certainly opens one, because an item read
//     where the page has none makes this function escape the author's
//     CLOSER and leave the real opener live.
//   - does this line continue the paragraph above it? An item paragraph
//     LAZILY continued at column 0 ends the item for trackItem where
//     CommonMark keeps it open, and a line that is no paragraph at all (a
//     heading, a table row) is read as one, so an uncertain marker under it
//     opens no item here where the page opens one.
//
// Measured against the renderer, over 14580 generated field shapes rendered
// by pandoc 3.1.3 gfm with a sentinel line spliced after the escaped text:
// 123 leak, EVERY one of them carrying a delimiter four or more columns
// in — nothing outside the first question, which is what that corpus
// exercises (it opens no container above the fence). The third question's
// shapes were measured one by one, on pandoc 3.1.3 AND GitHub's /markdown
// API, which agree.
//
// Against the corpus it serves: 0 of 42 shipped manifest fields and 0 of
// 679 tracked .md outside vendor/ are read differently by this reading and
// two deliberately stricter ones, and no line of those 679 files or of the
// 196 tracked yaml carries a delimiter four columns in, or a delimiter a
// list marker holds, at all.
//
// Closing a question takes the block phase itself — a container stack
// advanced line by line, carrying CommonMark's interruption and lazy
// continuation rules. One more regular-expression spelling of the guess
// buys the next shape, not the class, so the limit is stated.
func EscapeDanglingFence(md string) string {
	lines := Lines(md)
	for from := 0; from < len(lines); {
		at, marker, delim := danglingFenceOpener(lines[from:])
		if at < 0 {
			break
		}
		at += from
		lines[at] = lines[at][:marker] +
			strings.Repeat(`\`+delim[:1], len(delim)) +
			lines[at][marker+len(delim):]
		from = at + 1
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
// altogether; the escaped form shows the pipe, in a span as well as in prose.
//
// A pipe the TEXT already escapes is left alone, which is the rule the row's
// own reader applies: a pipe is a separator when the run of backslashes
// before it is even. Escaping it again writes `\\|` — an escaped backslash
// followed by a LIVE separator — and that breaks the very cell this repairs:
// measured on pandoc 3.1.3 gfm, `Revi \| the reviewer` escaped twice gains a
// column and drops the row's last cell, where the author's own `\|` keeps
// its three and renders the pipe.
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
			// the opener the container already closed — one escape too many,
			// written into the author's words, and the real opener left
			// live.
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
// every reader of this package sees the quoted text as prose — so a link form
// the page only quoted is read as a link, refused, and the generator that was
// cutting the sentence fails on a page whose links are sound.
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
		// directly, the closing run would MERGE with the trailing one into a
		// single wider run, which closes nothing.
		sep := ""
		if s[len(s)-1] == '`' {
			sep = " "
		}
		return s + sep + strings.Repeat("`", j-i)
	}
	return s
}
