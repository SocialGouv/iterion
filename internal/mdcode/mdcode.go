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
package mdcode

import (
	"regexp"
	"strings"
)

var (
	// spanRe matches an inline code span: a run of backticks, the text it
	// opens, and the run that closes it. ``a `b` c`` and `x` are both spans;
	// a run with no closing run is not one, which is what a renderer does
	// with a stray backtick.
	spanRe = regexp.MustCompile("`+[^`]*`+")
	// fenceRe matches the delimiter line of a fenced code block, including
	// one inside a blockquote. Indentation is not bounded: a fence continued
	// inside a list item is still a fence.
	fenceRe = regexp.MustCompile("^\\s*(?:>\\s?)*(`{3,}|~{3,})")
)

// SpanPattern returns the inline-code-span pattern. It exists so a scanner
// that must strip or transform a span rather than mask it — a heading
// reduced to the text GitHub slugs, say — uses this rule and not a second
// one that drifts from it.
func SpanPattern() *regexp.Regexp { return spanRe }

// FencePattern returns the fenced-block delimiter pattern, capturing the
// delimiter itself in group 1: a caller that tracks open/close state needs
// the marker, since a fence closes only on a run of the same character at
// least as long as the one that opened it.
func FencePattern() *regexp.Regexp { return fenceRe }

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
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			marker := m[1]
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
			for _, r := range spanRe.FindAllStringIndex(line, -1) {
				blank(pos+r[0], pos+r[1])
			}
		}
		pos = next
	}
	return string(out)
}
