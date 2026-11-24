package author

import (
	"bytes"
	"strings"

	"gopkg.in/yaml.v3"
)

// markerRegions are the parts of an author document the writer re-emits off
// the source rather than off the program: the explicit document markers the
// author wrote, with the lines written around them.
//
// A `---` opening the document, and the trailing region below the
// document's last content line — the `...` markers, the comments and the
// blank lines written there — hang on no node: the source is the one place
// each of those lines is written, which is where Comments reads them, by
// position. So the writer copies the region back verbatim, and the comments
// yaml.v3 attached FROM that region are taken off the tree before the
// encode (stripTrailingRegion): a line carried onto a node as well would be
// written twice.
//
// The `%YAML`/`%TAG` directives of a stream are the one thing here the
// writer does not re-emit — a program does not describe a directive, and
// the document the writer renders is one document, not a stream — and they
// do not disappear in silence either: Directives lists them, and the
// canonical form refuses a document carrying one, naming it (ADR-102 §5).
type markerRegions struct {
	// start is the document-start marker line the source opened with, ""
	// when it opened with none. The comment of that line is not taken with
	// it: yaml.v3 hangs it on the document's head, which the writer emits
	// under the marker it re-emits.
	start string
	// trailing are the lines below the document's last content line,
	// verbatim, without their line breaks.
	trailing []string
}

// readMarkerRegions reads off lines — the source of the document yaml.v3
// read as doc — the regions the writer re-emits. doc positions the trailing
// region: a block scalar's body, where a line opening with a `#` is the
// author's prose and no comment at all, is content (contentEnd). The lines
// are the caller's, read once for this and for the strip, so the two
// cannot read a region off two different texts.
func readMarkerRegions(lines []string, doc *yaml.Node) markerRegions {
	trailing := lines[min(contentEnd(lines, doc), len(lines)):]
	// The final element of a source ending with a line break is the empty
	// text after it, not a blank line of the region: emitted, it would add
	// one.
	if n := len(trailing); n > 0 && trailing[n-1] == "" {
		trailing = trailing[:n-1]
	}
	return markerRegions{start: documentStartMarker(lines), trailing: trailing}
}

// emit renders the regions around written, the text the encoder produced
// for the program: the start marker above it, the trailing region below it.
// written ends with a line break, as the encoder's text does, and so does
// the result.
func (m markerRegions) emit(written []byte) []byte {
	if m.start == "" && len(m.trailing) == 0 {
		return written
	}
	var b strings.Builder
	if m.start != "" {
		b.WriteString(m.start + "\n")
	}
	b.Write(written)
	for _, l := range m.trailing {
		b.WriteString(l + "\n")
	}
	return []byte(b.String())
}

// documentStartMarker is the `---` line the document opens with, "" when it
// opens with none: a `---` at column 0 with nothing but directives,
// comments and blank lines above it. The marker alone comes back, never the
// comment written on its line — Comments lists that comment once, and
// yaml.v3 hands it to the writer on the document's head.
func documentStartMarker(lines []string) string {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case isDocStart(l):
			return "---"
		case t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(l, "%"):
		default:
			return ""
		}
	}
	return ""
}

// isDocStart reports whether line is a `---` document-start marker: the
// indicator at column 0, followed by a blank or the end of its line, which
// is where YAML reads one — as isDocEnd reads a `...`. Indented or glued to
// another character it is text.
func isDocStart(line string) bool {
	return strings.HasPrefix(line, "---") && (len(line) == 3 || line[3] == ' ' || line[3] == '\t')
}

// Directives lists the `%YAML`/`%TAG` directive lines src opens its stream
// with, trimmed, in source order — nil for a document opening with none.
// The writer emits no directive, so a rewrite would drop one: the canonical
// form refuses the document instead, naming it, rather than losing a line
// the author wrote in silence (ADR-102 §5).
// Read off the head of src line by line, never off a split of the whole
// text: a document is bounded by 10 MiB, and its directives, if any, are on
// its first lines.
func Directives(src []byte) []string {
	var out []string
	rest := bytes.TrimPrefix(src, []byte("\ufeff"))
	for len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		t := strings.TrimSpace(string(line))
		switch {
		case len(line) > 0 && line[0] == '%':
			out = append(out, t)
		case t == "" || strings.HasPrefix(t, "#"):
		default:
			return out
		}
	}
	return out
}

// stripTrailingRegion clears off doc the comment lines yaml.v3 attached from
// the document's trailing region, which the writer re-emits verbatim
// (markerRegions): a line left on a node as well would be written twice.
//
// They are the lines Comments does not list twice, read the same way — the
// run of node feet the walk closes on that the region's head repeats
// (hungOnTheLastNode) — and the document's own foot keeps the lines written
// ABOVE the region, which the region's read never saw (footAboveTheRegion).
func stripTrailingRegion(doc *yaml.Node, lines []string) {
	trailing := trailingComments(lines, doc)
	if len(trailing) == 0 {
		return
	}
	foot := doc.FootComment
	doc.FootComment = ""
	w := walkComments(doc, lines)
	for i := len(w.lines) - hungOnTheLastNode(w.lines, w.footRun, trailing); i < len(w.lines); i++ {
		dropCommentLine(w.blocks[i], w.lines[i])
	}
	doc.FootComment = strings.Join(footAboveTheRegion(foot, trailing), "\n")
}

// dropCommentLine removes from block the last line saying text. A nil block
// — a line the walk read off a flow collection's own source, which no
// comment block holds — is left alone: the run this walks is a run of node
// FEET, so none reaches here, and one that did would be written twice and
// refuse the rewrite rather than be lost.
func dropCommentLine(block *string, text string) {
	if block == nil {
		return
	}
	ls := strings.Split(*block, "\n")
	for i := len(ls) - 1; i >= 0; i-- {
		if strings.TrimSpace(ls[i]) == text {
			*block = strings.Join(append(ls[:i:i], ls[i+1:]...), "\n")
			return
		}
	}
}
