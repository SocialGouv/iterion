// Package rewrite holds the primitives a surgical rewrite of a `.bot` text
// needs — the text the lexer reads (BOM stripped, CRLF folded) with the way
// back to the original bytes, non-overlapping byte edits, and the line and
// column arithmetic — so the profile migration and the diagnostic fixes cut
// the same way and never through the writer.
package rewrite

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// Edit replaces the bytes [Start, End) of the normalised text with Repl.
type Edit struct {
	Start, End int
	Repl       string
}

// Apply rewrites text; edits are sorted by Start and must not overlap. An
// overlap is a defect of the planning, reported — never sliced into a file,
// and never a panic out of a CI gate.
func Apply(text string, edits []Edit) (string, error) {
	var b strings.Builder
	at := 0
	for _, e := range edits {
		if e.Start < at || e.End < e.Start || e.End > len(text) {
			return "", fmt.Errorf("overlapping edits at byte %d (the previous one ended at %d): a defect of the rewrite, not of the file", e.Start, at)
		}
		b.WriteString(text[at:e.Start])
		b.WriteString(e.Repl)
		at = e.End
	}
	b.WriteString(text[at:])
	return b.String(), nil
}

// Normalized is the text the lexer reads — BOM stripped, CRLF folded —
// with what it takes to map an offset back to the original bytes.
type Normalized struct {
	Text string
	bom  int   // bytes of BOM removed at the start
	crlf []int // offsets, in Text, of the '\n' that had a '\r' before it
}

// Normalize reads src the way the lexer does.
func Normalize(src []byte) Normalized {
	n := Normalized{}
	s := string(src)
	if strings.HasPrefix(s, "\ufeff") {
		n.bom = len("\ufeff")
		s = s[n.bom:]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			n.crlf = append(n.crlf, b.Len())
			continue // the '\n' that follows is written on the next turn
		}
		b.WriteByte(s[i])
	}
	n.Text = b.String()
	return n
}

// OrigOffset maps an offset of the normalised text to the original bytes:
// the BOM, and one '\r' per folded newline strictly before it, are added
// back — so an offset ON a folded '\n' maps to its '\r', and a range ending
// there keeps the file's own line ending.
func (n Normalized) OrigOffset(o int) int {
	k := sort.SearchInts(n.crlf, o) // folded newlines strictly before o
	return o + n.bom + k
}

// MapBack rebuilds the rewritten file on the ORIGINAL bytes: everything
// outside the edited ranges is copied as it was (BOM and line endings
// included), each edit's replacement is written in place, and a
// replacement's own line endings follow the file's.
func (n Normalized) MapBack(src []byte, edits []Edit) []byte {
	eol := "\n"
	if len(n.crlf) > 0 {
		eol = "\r\n"
	}
	var out bytes.Buffer
	at := 0
	for _, e := range edits {
		s, t := n.OrigOffset(e.Start), n.OrigOffset(e.End)
		out.Write(src[at:s])
		out.WriteString(strings.ReplaceAll(e.Repl, "\n", eol))
		at = t
	}
	out.Write(src[at:])
	return out.Bytes()
}

// LineStarts is the byte offset of each line's first byte.
func LineStarts(text string) []int {
	starts := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// LineSpan is the byte range of a 1-based line, its newline included.
func LineSpan(text string, starts []int, line int) (int, int) {
	start := starts[line-1]
	end := len(text)
	if line < len(starts) {
		end = starts[line]
	}
	return start, end
}

// ColumnByte is the byte offset within lineText of its 1-based rune column.
func ColumnByte(lineText string, column int) int {
	if column <= 1 {
		return 0
	}
	n := 0
	for i := range lineText {
		n++
		if n == column {
			return i
		}
	}
	return len(lineText)
}

// RuneByteOffsets maps a rune index of text to its byte offset (one more
// entry than runes, for an exclusive end).
func RuneByteOffsets(text string) []int {
	out := make([]int, 0, len(text)+1)
	for i := range text {
		out = append(out, i)
	}
	return append(out, len(text))
}
