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
	var (
		inFence   bool
		fenceChar byte
		fenceLen  int
	)
	for pos := 0; pos <= len(md); {
		line, next := md[pos:], len(md)+1
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line, next = line[:nl], pos+nl+1
		}
		delimiter := false
		if marker := FenceMarker(line); marker != "" {
			switch {
			case !inFence:
				inFence, fenceChar, fenceLen, delimiter = true, marker[0], len(marker), true
			default:
				rest := strings.TrimSpace(line[strings.Index(line, marker)+len(marker):])
				if marker[0] == fenceChar && len(marker) >= fenceLen && rest == "" {
					inFence, delimiter = false, true
				}
			}
		}
		if inFence || delimiter {
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

// CloseDanglingSpan repairs a string whose last backtick run opens a code
// span the string does not close, by appending the run that closes it.
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
// drops the very name the sentence was quoting. Closing keeps every byte the
// truncation kept and costs at most a few backticks.
func CloseDanglingSpan(s string) string {
	end := 0
	for _, r := range Spans(s) {
		end = r[1]
	}
	i := strings.IndexByte(s[end:], '`')
	if i < 0 {
		return s
	}
	open := end + i
	n := 0
	for j := open; j < len(s) && s[j] == '`'; j++ {
		n++
	}
	// A run with nothing after it opened nothing worth keeping: drop it
	// rather than close an empty span.
	if strings.TrimSpace(s[open+n:]) == "" {
		return s[:open]
	}
	return s + strings.Repeat("`", n)
}
