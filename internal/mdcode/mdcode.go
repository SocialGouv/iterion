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
	// fenceRe matches the delimiter line of a fenced code block, including
	// one inside a blockquote. Indentation is not bounded: a fence continued
	// inside a list item is still a fence.
	fenceRe = regexp.MustCompile("^\\s*(?:>\\s?)*(`{3,}|~{3,})")
)

// SpanPattern returns the one-regexp approximation of a code span. Prefer
// Spans, which implements the pairing rule; this exists for a caller that
// must replace a span's TEXT — a heading reduced to what GitHub slugs — and
// has always read the approximation.
func SpanPattern() *regexp.Regexp { return spanRe }

// FencePattern returns the fenced-block delimiter pattern, capturing the
// delimiter itself in group 1: a caller that tracks open/close state needs
// the marker, since a fence closes only on a run of the same character at
// least as long as the one that opened it.
func FencePattern() *regexp.Regexp { return fenceRe }

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

// FenceMarker returns the fenced-block delimiter a line carries, or "".
//
// A backtick fence's info string may not itself contain a backtick: "```go
// `x`" is a PARAGRAPH, not a fence. Reading one as an opener costs the whole
// rest of the page, because no later line closes it.
func FenceMarker(line string) string {
	m := fenceRe.FindStringSubmatch(line)
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
// The zero value is ready: a document starts outside a fence.
type FenceScanner struct {
	open   bool
	char   byte
	n      int
	quote  int
	indent int
}

// Open reports whether the scanner is currently inside a fenced block — the
// state left by the lines already fed to Code, before the next one is read.
func (f *FenceScanner) Open() bool { return f.open }

// Code reports whether the line is code — a fence delimiter, or a line inside
// a fenced block — and advances the scanner. A delimiter is code too: it is
// not prose, and no user of this type has ever wanted it.
func (f *FenceScanner) Code(line string) bool {
	marker := FenceMarker(line)
	if marker == "" {
		return f.open
	}
	quote, indent := fencePrefix(line, marker)
	if !f.open {
		f.open, f.char, f.n, f.quote, f.indent = true, marker[0], len(marker), quote, indent
		return true
	}
	// A fence closes only on a run of the SAME character, at least as long as
	// the one that opened it, with nothing after it, at the SAME blockquote
	// depth and no further than three columns in. Anything else is content —
	// and the delimiter of a block a page is SHOWING is content by exactly
	// this rule: `> ``` ` closing a block opened at depth 0 is a quote of a
	// closer, not a closer.
	rest := strings.TrimSpace(line[strings.Index(line, marker)+len(marker):])
	if quote == f.quote && indent <= f.indent+3 &&
		marker[0] == f.char && len(marker) >= f.n && rest == "" {
		f.open = false
	}
	return true
}

// fencePrefix reads the blockquote depth and the indentation out of what
// FenceMarker's pattern consumed before the marker.
func fencePrefix(line, marker string) (quote, indent int) {
	pre := line[:strings.Index(line, marker)]
	quote = strings.Count(pre, ">")
	if i := strings.LastIndex(pre, ">"); i >= 0 {
		pre = pre[i+1:]
	}
	return quote, len(pre)
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
		line, next := md[pos:], len(md)+1
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line, next = line[:nl], pos+nl+1
		}
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
// The caller owes a SINGLE line: Spans pairs runs across a newline while
// Mask reasons per line, so a repair spanning one would satisfy Spans and
// still show backticks on the page. Both callers collapse newlines first.
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
