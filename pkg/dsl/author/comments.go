package author

import (
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Comments lists the YAML comments src carries — head, line and foot
// comments of every node, one entry per comment line, in document order: a
// node's head and line comments, then its children's, then its foot
// comment; in a mapping, pair by pair — yaml.v3 hangs the foot comment of
// `key: value` on the key, and it comes below every comment of the value,
// the value's line comment included. Inside a flow collection yaml.v3 keeps
// only some of them (one right after the opening bracket is dropped) and
// hangs the one after the closing bracket on the collection: a flow
// collection's comments are read off its source, in order, that one last.
// Below the document's last content line — around a `...` document-end
// marker, or under the last declaration — it attaches some of the lines
// written there and drops the rest: that region is read off the source too,
// last of all.
// Write renders the program, not the document, and keeps none of them;
// WriteKeepingComments carries them onto the nodes it writes, and `iterion
// fmt` compares this list for the source and the written form before it
// rewrites a document in its canonical form, refusing when one would be
// lost; `fmt --to bot` counts them and names the first. A source that is
// not YAML has no comments to report.
func Comments(src []byte) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil
	}
	lines := sourceLines(src)
	// The document's foot is taken off the node and handed to the one
	// reader of the trailing region's source, which is where every line of
	// that region is counted.
	foot := doc.FootComment
	doc.FootComment = ""
	var out []string
	// footRun counts the lines at the END of out the FOOT comments the walk
	// CLOSES on added: a comment of the trailing region hangs on a foot and
	// on nothing else, so those are the only lines its source read repeats.
	// Visiting any node afterwards ends the run — whether that node carries
	// a comment or not, since a foot run armed past a comment-less node
	// would match the region's head off a comment written elsewhere.
	footRun := 0
	add := func(block string, isFoot bool) {
		if !isFoot {
			footRun = 0
		}
		added := commentLines(block)
		if len(added) == 0 {
			return
		}
		out = append(out, added...)
		if isFoot {
			footRun += len(added)
		}
	}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Style&yaml.FlowStyle != 0 && (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) {
			flow, hungFoot := walkFlow(n, lines)
			if len(flow) == 0 {
				footRun = 0
				return
			}
			out = append(out, flow...)
			if len(flow) == hungFoot {
				footRun += hungFoot
			} else {
				footRun = hungFoot
			}
			return
		}
		add(n.HeadComment, false)
		add(n.LineComment, false)
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				add(k.HeadComment, false)
				add(k.LineComment, false)
				for _, c := range k.Content {
					walk(c)
				}
				walk(n.Content[i+1])
				add(k.FootComment, true)
			}
		} else {
			for _, c := range n.Content {
				walk(c)
			}
		}
		add(n.FootComment, true)
	}
	walk(&doc)
	trailing := trailingComments(lines, &doc)
	out = out[:len(out)-hungOnTheLastNode(out, footRun, trailing)]
	// A comment on a directive's line (`%YAML 1.1 # c`, `%TAG …`) is no
	// node's: yaml.v3 drops it. It is read off the source and listed after
	// the own-line comments written above the first directive, which
	// yaml.v3 lists first, on the document's head.
	if above, dc := directiveComments(lines); len(dc) > 0 {
		at := min(above, len(out))
		out = append(out[:at], append(dc, out[at:]...)...)
	}
	return append(out, append(footAboveTheRegion(foot, trailing), trailing...)...)
}

// commentLines cuts a yaml.v3 comment block into its lines, trimmed, the
// blank ones out — a head comment carries the blanks written inside it.
func commentLines(block string) []string {
	var out []string
	for _, line := range strings.Split(block, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// walkFlow lists a flow collection's comments from its SOURCE, in written
// order — the source is the single source of truth, and no comment is
// listed twice. yaml.v3 hangs some of these comments on the node's head,
// line and foot as well; the sourced line each one physically sits on is
// what tells a hung copy (already listed from its own source line) from a
// distinct comment:
//
//   - the head's lines are the contiguous comment block ending above the
//     OPENING bracket's line (blanks skipped — a head comment may carry
//     them); a sourced read on one of those lines is the head's, emitted
//     there and nowhere else;
//   - a line or foot comment whose text the sourced read already emitted
//     is the moved inside comment (the `[a: # c` + break shapes) — as many
//     copies of it as the source writes in the region THAT slot is filled
//     from aside: the CLOSING bracket's own line for the node's line
//     comment, the own-line comment block below it for its foot. Those are
//     the author's own separate comments and are listed; a copy beyond
//     them is the moved one, listed already from its own source line, and
//     a same text written anywhere else belongs to the node it is written
//     under;
//   - anything else — a comment between a `!` tag and its bracket the head
//     never carried, a same-text comment on another line — is listed, same
//     text or not (the one-short fuzz case).
//
// A head whose block cannot be found above the opening bracket is still
// listed, from the hung text: failing to find it must not lose it. The
// lines the node's FOOT comment contributed come back with the list, as
// their count: they close it, and they are the ones a comment written below
// the document's last content line can be hung on.
func walkFlow(n *yaml.Node, lines []string) (out []string, hungFoot int) {
	before, inside, openLine, closeLine := flowComments(lines, n)
	headLines := headCommentLines(lines, openLine, n.HeadComment)
	sourced := append(append([]flowComment{}, before...), inside...)
	sort.Slice(sourced, func(i, j int) bool { return sourced[i].line < sourced[j].line })
	if len(headLines) > 0 {
		heads := make([]int, 0, len(headLines))
		for l := range headLines {
			heads = append(heads, l)
		}
		sort.Ints(heads)
		for _, l := range heads {
			if t := strings.TrimSpace(lines[l-1]); t != "" {
				out = append(out, t)
			}
		}
	} else {
		out = append(out, commentLines(n.HeadComment)...)
	}
	sourcedText := map[string]bool{}
	for _, c := range sourced {
		if headLines[c.line] {
			continue
		}
		out = append(out, c.text)
		sourcedText[c.text] = true
	}
	for i, hung := range []string{n.LineComment, n.FootComment} {
		written := map[string]int{}
		for _, t := range commentLines(hung) {
			if sourcedText[t] {
				// The slot holds the inside comment yaml.v3 moved onto the
				// node AND the ones written after the close: of a text the
				// sourced read already listed, emit as many copies as the
				// region the slot is filled from writes, and no more.
				written[t]++
				if written[t] > commentsAfterClose(t, lines, closeLine, i == 1) {
					continue
				}
			}
			out = append(out, t)
			if i == 1 {
				hungFoot++
			}
		}
	}
	return out, hungFoot
}

// commentsAfterClose counts the comments of this text the source writes in
// the region the slot it hangs from is filled from — physical comments the
// sourced read, which stops at the closing bracket, never saw. yaml.v3 fills
// those two slots from two places and no other: a flow collection's LINE
// comment from the closing bracket's own line, its FOOT comment from the
// own-line comment block written below that line (blank lines in between
// included). A hung copy beyond that count is the inside comment yaml.v3
// moved onto the node's line (#1814), which the sourced read already listed.
// With no closing line in the source — closeLine 0, or one past the last
// line — the count is 0: the sourced read then ran to the end of the source,
// so every hung copy of a text it emitted is that moved comment.
func commentsAfterClose(text string, lines []string, closeLine int, foot bool) int {
	if closeLine < 1 || closeLine > len(lines) {
		return 0
	}
	if !foot {
		if lineComment(lines[closeLine-1], text) {
			return 1
		}
		return 0
	}
	n := 0
	for li := closeLine; li < len(lines); li++ {
		if t := strings.TrimSpace(lines[li]); t != "" && !strings.HasPrefix(t, "#") {
			break
		}
		if lineComment(lines[li], text) {
			n++
		}
	}
	return n
}

// lineComment reports whether line carries a comment of exactly this text.
// EVERY `#` written after a blank is a candidate, the last character of the
// line included: the first one may sit inside a quoted value
// (`b: "a # b"} # c`) and the comment after it, and a comment may be a bare
// `#`.
func lineComment(line, text string) bool {
	r := []rune(line)
	for ci := 0; ci < len(r); ci++ {
		if r[ci] == '#' && (ci == 0 || r[ci-1] == ' ' || r[ci-1] == '\t') &&
			strings.TrimSpace(string(r[ci:])) == text {
			return true
		}
	}
	return false
}

// trailingComments lists, in written order, every comment written below the
// document's last content line: its own-line comments, whatever their
// indentation and the blank lines between them, and the comment on a `...`
// document-end marker's own line. yaml.v3 attaches SOME of those lines — to
// the document's foot, or to the foot of a node they are written under, one
// block over two nodes included — and reads the rest without attaching them
// to anything; the writer emits no marker, so a block written around one has
// no place of its own in a written form either. The source is the one place
// each of those lines is written, so they are all read from there, by
// position: the same text written twice is two comments, and a rewrite that
// kept one would lose the other.
func trailingComments(lines []string, doc *yaml.Node) []string {
	var out []string
	for li := contentEnd(lines, doc); li < len(lines); li++ {
		switch t := strings.TrimSpace(lines[li]); {
		case strings.HasPrefix(t, "#"):
			out = append(out, t)
		case isDocEnd(lines[li]):
			if c := trailingComment(lines[li], 3); c != "" {
				out = append(out, c)
			}
		}
	}
	return out
}

// contentEnd is the 0-based line the trailing region starts at: below the
// last line that is neither blank, nor an own-line comment, nor a `...`
// document-end marker — and below the last line of every MULTI-LINE scalar,
// block or quoted, where a line starting with `#` is the author's text and
// no comment at all.
func contentEnd(lines []string, doc *yaml.Node) int {
	end := 0
	for li := len(lines) - 1; li >= 0; li-- {
		if t := strings.TrimSpace(lines[li]); t == "" || strings.HasPrefix(t, "#") || isDocEnd(lines[li]) {
			continue
		}
		end = li + 1
		break
	}
	return max(end, multiLineScalarEnd(lines, doc))
}

// multiLineScalarEnd is the 0-based line below the last one a scalar that may
// span several lines occupies, over every such scalar doc holds: a block
// scalar's body, and a quoted scalar's text up to its closing quote. A plain
// scalar needs none of it — YAML ends one at a ` #`, so a line of a plain
// scalar never starts with a `#` that is not a comment.
//
// A block scalar's body is read with the indentation of the block its node
// sits in, which is what YAML counts a body's own indentation from: the
// 0-based column of a block mapping's or sequence's first line, and -1 at
// the document's root, where the scanner starts.
func multiLineScalarEnd(lines []string, doc *yaml.Node) int {
	end := 0
	var walk func(n *yaml.Node, block int)
	walk = func(n *yaml.Node, block int) {
		if n == nil {
			return
		}
		if n.Kind == yaml.ScalarNode {
			switch {
			case n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
				end = max(end, blockBodyEnd(lines, n.Line, n.Column, block))
			case n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0:
				end = max(end, quotedScalarEnd(lines, n))
			}
		}
		inner := block
		if n.Style&yaml.FlowStyle == 0 && (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) {
			inner = n.Column - 1
		}
		for _, c := range n.Content {
			walk(c, inner)
		}
	}
	walk(doc, -1)
	return end
}

// blockBodyEnd is the 0-based line below the last one the body of the block
// scalar whose `|` or `>` indicator sits on the 1-based line indicator, at
// its 1-based column col, inside the block whose 0-based indentation is
// block (-1 at the document's root), occupies: the body opens on the line
// below the indicator and runs, as YAML reads it, while a line is blank or
// indented at least as much as the body's own indentation. That indentation
// is the one the indicator declares (`|2`) counted from the block's, or, when
// it declares none, the first non-blank line's — floored, as the scanner
// floors it, at the block's indentation plus one and at column 1, so a line
// at column 0 (a `...` marker, a comment, the document that follows) or one
// no deeper than the block opens the trailing region and is no body line.
//
// A declaration the indicator's line does not carry is not found: a property
// (`&a`, `!!str`) can hold the node's position a line ABOVE the indicator,
// and the body is then read short — a refusal, never a comment dropped in
// silence (TestABlockScalarUnderAPropertyLineReadsItsDeclaredBodyShort).
func blockBodyEnd(lines []string, indicator, col, block int) int {
	end, indent := min(indicator, len(lines)), -1
	if indicator >= 1 && indicator <= len(lines) {
		if declared := declaredBlockIndent(lines[indicator-1], col); declared > 0 {
			indent = declared
			if block >= 0 {
				indent += block
			}
		}
	}
	for li := indicator; li >= 0 && li < len(lines); li++ {
		if strings.TrimSpace(lines[li]) == "" {
			continue
		}
		at := lineIndent(lines[li])
		if indent < 0 {
			indent = max(at, block+1, 1)
		}
		if at < indent {
			break
		}
		end = li + 1
	}
	return end
}

// quotedScalarEnd is the 0-based line below the one that closes quoted scalar
// n, and 0 when the scan finds no closing quote: every line such a scalar
// spans is the author's text, so a line of one that starts with `#` is no
// comment. The scan indexes CHARACTERS, which is what yaml.v3 counts the
// column it starts from in — a multibyte character written before the value
// moves a byte index off it. The opening quote is the one openingQuote
// finds, and from there `\` escapes the next character of a double-quoted
// scalar while a doubled `'` is a literal quote inside a single-quoted one,
// as in flowComments' scan. Answering 0 leaves the region where the backward
// scan put it: the conservative end, never one below a comment.
func quotedScalarEnd(lines []string, n *yaml.Node) int {
	if n.Line < 1 || n.Line > len(lines) {
		return 0
	}
	quote := '"'
	if n.Style&yaml.SingleQuotedStyle != 0 {
		quote = '\''
	}
	li, ci := openingQuote(lines, n.Line-1, max(n.Column-1, 0), quote)
	if li < 0 {
		return 0
	}
	for ; li < len(lines); li++ {
		line := []rune(lines[li])
		for ; ci < len(line); ci++ {
			switch {
			case quote == '"' && line[ci] == '\\':
				ci++
			case line[ci] != quote:
			case quote == '\'' && ci+1 < len(line) && line[ci+1] == '\'':
				ci++
			default:
				return li + 1
			}
		}
		ci = 0
	}
	return 0
}

// openingQuote is the 0-based line a quoted scalar opens on and the
// CHARACTER index just past its opening quote, scanned forward from the
// 0-based line li and the character index ci — the position yaml.v3 gives
// the scalar, which it puts on an anchor (`&a`) or a tag (`!!str`) when the
// scalar carries one, so the quote may open a line BELOW it. Only those
// properties, blank space and comments can sit in between: a property token
// is skipped whole — a tag name being allowed a `'` of its own, the verbatim
// form `!<…>` ending at its `>` — and a `#` opening a line or written after a
// blank runs to the end of its line and is skipped too, so a quote inside
// either is not taken for the scalar's. Line -1 when no line holds one.
func openingQuote(lines []string, li, ci int, quote rune) (int, int) {
	for ; li < len(lines); li++ {
		line := []rune(lines[li])
		for ; ci < len(line); ci++ {
			switch {
			case line[ci] == quote:
				return li, ci + 1
			case line[ci] == '&' || line[ci] == '!':
				ci = propertyEnd(line, ci)
			case line[ci] == '#' && (ci == 0 || line[ci-1] == ' ' || line[ci-1] == '\t'):
				ci = len(line)
			}
		}
		ci = 0
	}
	return -1, 0
}

// declaredBlockIndent is the indentation the block scalar header at the
// 1-based column col of line declares — the digit of `|2`, `>2-`, `|-2` —
// and 0 when it declares none. The digit is counted, where the body is read,
// from the indentation of the enclosing block (blockBodyEnd adds it), which
// is where the scanner counts it from. The column is read in CHARACTERS,
// which is what yaml.v3 counts it in: a multibyte character written before
// the header would move a byte index of it back into the key. yaml.v3 puts a
// block scalar's column on its `|` or `>` indicator, an anchor (`&a`) or a
// tag (`!!str`) between the two aside, so the header is read from there: a
// `|` or a `>` written in the key, in the value or in the comment of the
// header's line is never one. A property the scan walks over is skipped
// whole, propertyEnd's own reading, so the `>` that CLOSES a verbatim tag
// (`!<a>`) is no folded indicator — and neither an anchor name nor a tag
// name can carry a `|` or a `>`, so skipping one never steps over the
// header the line declares.
//
// The header ENDS at its comment, and so does each half of the scan. The
// scanner eats the indicators, then blanks, then reads a `#` with NO blank
// required before it (yaml_parser_scan_block_scalar), so `|2#c` declares 2
// and comments `#c`. A `#` met before any indicator ends the line the way
// it ends the three other scans of this file that walk a property: the
// indicator such a line carries none of sits below it, held there by a
// property, and the comment's own `|9` declares nothing.
func declaredBlockIndent(line string, col int) int {
	r := []rune(line)
	at := min(max(col-1, 0), len(r))
	indicator := -1
	for ci := at; ci < len(r); ci++ {
		if r[ci] == '#' && (ci == 0 || r[ci-1] == ' ' || r[ci-1] == '\t') {
			break
		}
		if r[ci] == '&' || r[ci] == '!' {
			ci = propertyEnd(r, ci)
			continue
		}
		if r[ci] == '|' || r[ci] == '>' {
			indicator = ci
			break
		}
	}
	if indicator < 0 {
		return 0
	}
	indent := 0
	for ci := indicator + 1; ci < len(r); ci++ {
		switch c := r[ci]; {
		case c >= '0' && c <= '9':
			indent = indent*10 + int(c-'0')
		case c == '+' || c == '-':
		case c == ' ' || c == '\t' || c == '#':
			return indent
		default:
			return 0
		}
	}
	return indent
}

// lineIndent is the number of blanks line opens with.
func lineIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

// hungOnTheLastNode is the number of lines at the end of walked the
// trailing region's source read holds as well: a comment of that region
// yaml.v3 hung on a node's FOOT comment is listed at the end of the walk,
// and read again off the source. Matched as a run against the head of the
// region, over footRun — the lines of the feet the walk CLOSES on, and of
// those alone — so a comment hung on a node the walk left behind stays
// listed where it is, whatever its text.
func hungOnTheLastNode(walked []string, footRun int, trailing []string) int {
	for k := min(footRun, min(len(walked), len(trailing))); k > 0; k-- {
		if slices.Equal(walked[len(walked)-k:], trailing[:k]) {
			return k
		}
	}
	return 0
}

// footAboveTheRegion lists the lines of the document's foot comment the
// trailing region's source read does not hold — a foot written above that
// region, which the read never saw. Listed rather than dropped: a comment
// nothing lists is a comment a rewrite loses in silence.
func footAboveTheRegion(foot string, trailing []string) []string {
	held := map[string]int{}
	for _, c := range trailing {
		held[c]++
	}
	var out []string
	for _, c := range commentLines(foot) {
		if held[c] > 0 {
			held[c]--
			continue
		}
		out = append(out, c)
	}
	return out
}

// isDocEnd reports whether line is a `...` document-end marker: the
// indicator at column 0, followed by a blank or the end of its line, which
// is where YAML reads one. Indented (a block scalar's body, a value) or
// glued to another character (`...x`, a key of that name) it is text. At
// column 0 it closes the document, so anything after it but comments and
// blank lines is a second document — refused (E050) before any comment is
// counted.
func isDocEnd(line string) bool {
	return strings.HasPrefix(line, "...") && (len(line) == 3 || line[3] == ' ' || line[3] == '\t')
}

// trailingComment is the comment a `#` after a blank starts on line, read
// from rune from on, "" when the line carries none: a `#` with no blank
// before it is text, as YAML reads it (`a#b`).
func trailingComment(line string, from int) string {
	r := []rune(line)
	for ci := max(from, 1); ci < len(r); ci++ {
		if r[ci] == '#' && (r[ci-1] == ' ' || r[ci-1] == '\t') {
			return strings.TrimSpace(string(r[ci:]))
		}
	}
	return ""
}

// directiveComments reads the comments on the directive lines that open a
// YAML stream — a `#` after a blank on a line that starts with `%` — and
// counts the own-line comments written above the first of them. The scan
// ends at the first line that is none of a directive, a comment or a blank.
func directiveComments(lines []string) (above int, comments []string) {
	seen := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "%"):
			seen = true
			if c := trailingComment(l, 1); c != "" {
				comments = append(comments, c)
			}
		case t == "":
		case strings.HasPrefix(t, "#"):
			if !seen {
				above++
			}
		default:
			return above, comments
		}
	}
	return above, comments
}

// flowComments reads the comments inside flow collection n off the source
// lines, in order, from its opening bracket to the one that closes it, as
// yaml.v3's scanner reads them. Where a token may start, a `#` starts a
// comment to the end of its line, a quote opens a quoted scalar, an anchor
// (`&a`) or a tag (`!!str`) is a property of the value that follows and is
// skipped whole, `:` and `?` are indicators whatever follows them (in a
// flow collection), and any other character starts a plain scalar. Inside
// a plain scalar a quote is text (`it's`, `b:'c`), and so is a `#` with no
// blank before it (`a#b`); the scalar ends at `,`, `?`, a bracket, or a `:`
// followed by a blank or the end of its line, and goes on across blanks and
// line breaks otherwise.
// The comments before the opening bracket — the node starts at its `!` tag
// when it has one — are listed apart (before) from those inside it, each
// with whether it is written on its own line. The 1-based lines of the
// opening and closing brackets come back with them.
func flowComments(lines []string, n *yaml.Node) (before []flowComment, out []flowComment, openLine int, closeLine int) {
	depth, started, plain := 0, false, false
	openLine, closeLine = 0, 0
	var quote rune
	for li := n.Line - 1; li >= 0 && li < len(lines); li++ {
		line := []rune(lines[li])
		ci := 0
		if li == n.Line-1 {
			ci = max(n.Column-1, 0)
		}
		for ; ci < len(line); ci++ {
			r := line[ci]
			switch {
			case quote == '"':
				switch r {
				case '\\':
					ci++
				case '"':
					quote = 0
				}
				continue
			case quote == '\'':
				if r == '\'' {
					if ci+1 < len(line) && line[ci+1] == '\'' {
						ci++
					} else {
						quote = 0
					}
				}
				continue
			case !started:
				// Before the bracket: the node may start at a `!` tag
				// or a `&` anchor (yaml.v3 places a tagged or anchored
				// collection there), which is skipped whole — a tag
				// name may carry a `[` of its own — and a `#`
				// there starts a comment, a bracket inside it included.
				if r == '#' && (ci == 0 || line[ci-1] == ' ' || line[ci-1] == '\t') {
					before = append(before, flowComment{
						text:    strings.TrimSpace(string(line[ci:])),
						ownLine: strings.TrimSpace(string(line[:ci])) == "",
						line:    li + 1,
					})
					ci = len(line)
				} else if r == '&' || r == '!' {
					ci = propertyEnd(line, ci)
				} else if r == '[' || r == '{' {
					started, depth, openLine = true, 1, li+1
				}
				continue
			case r == ' ' || r == '\t':
				continue
			case r == '#' && (!plain || ci == 0 || line[ci-1] == ' ' || line[ci-1] == '\t'):
				out = append(out, flowComment{text: strings.TrimSpace(string(line[ci:])), line: li + 1})
				plain = false
				ci = len(line)
				continue
			case plain && !endsPlain(line, ci):
				continue
			}
			// A token starts at r, or r ends the plain scalar before it.
			plain = false
			switch r {
			case '"', '\'':
				quote = r
			case '[', '{':
				depth++
			case ']', '}':
				depth--
				if depth == 0 {
					return before, out, openLine, li + 1
				}
			case ',', ':', '?':
			case '&', '!':
				// An anchor or a tag is a PROPERTY of the value that
				// follows, not a scalar of its own: the token is skipped
				// whole, so the quote behind it opens the scalar it
				// belongs to rather than reading as text.
				ci = propertyEnd(line, ci)
			default:
				plain = true
			}
		}
	}
	return before, out, openLine, closeLine
}

// flowComment is a comment read off a flow collection's source: before
// its opening bracket, or inside it — with the 1-based line its `#` sits
// on, the position a yaml.v3-hung copy is told apart from (#1814).
type flowComment struct {
	text    string
	ownLine bool // nothing but blanks before it on its line
	line    int
}

// headCommentLines maps the source lines the node's HEAD comment occupies:
// the contiguous block of comment lines whose texts are the head's, ending
// above the OPENING bracket's line, blanks skipped (a head comment may
// carry them). An empty map when the head is not written there — the
// caller falls back to the hung text, which it then lists alone.
func headCommentLines(lines []string, openLine int, head string) map[int]bool {
	out := map[int]bool{}
	if head == "" || openLine < 2 || openLine > len(lines) {
		return out
	}
	want := []string{}
	for _, l := range strings.Split(head, "\n") {
		t := strings.TrimSpace(l)
		if t != "" {
			want = append(want, t)
		}
	}
	li := openLine - 2 // the 0-based line above the opening bracket's line
	for i := len(want) - 1; i >= 0; i-- {
		for li >= 0 && strings.TrimSpace(lines[li]) == "" {
			li-- // a blank inside the head block is the head's too
		}
		if li < 0 || strings.TrimSpace(lines[li]) != want[i] {
			return map[int]bool{}
		}
		out[li+1] = true
		li--
	}
	return out
}

// propertyEnd is the index of the last rune of the anchor (`&a`) or tag
// (`!!str`, `!<a'b>`) token that opens at ci inside line, which the caller
// skips whole: the name runs while propertyRune accepts a character, so the
// token ends at the first blank, quote, `#`, `{` or `}` — and a `'`, a `,`,
// a `[` or a `]` written INSIDE a tag name belongs to it, where reading one
// as a quote or as a flow indicator makes the author's own prose look like a
// comment.
//
// The VERBATIM form `!<…>` is DELIMITED instead of being read by character
// class (yaml_parser_scan_tag eats `!<` and reads the URI up to its `>`):
// `<` is in neither class propertyRune carries, so the token ends at the `>`
// that closes it, whatever the name holds. A `!<` no `>` closes on its line
// is a tag yaml.v3 refuses — "did not find the expected '>'" — so no
// document a scan of this file reads carries one; the rest of the line is
// skipped, the conservative end.
func propertyEnd(line []rune, ci int) int {
	tag := line[ci] == '!'
	if tag && ci+1 < len(line) && line[ci+1] == '<' {
		for i := ci + 2; i < len(line); i++ {
			if line[i] == '>' {
				return i
			}
		}
		return len(line) - 1
	}
	for ; ci+1 < len(line); ci++ {
		if !propertyRune(line[ci+1], tag) {
			return ci
		}
	}
	return ci
}

// propertyRune reports whether r may appear inside a property name, as the
// vendored yaml.v3 scanner reads one: an alphanumeric, `_` or `-` for an
// anchor or an alias (yaml_parser_scan_anchor reads is_alpha), those plus
// the tag-URI punctuation for a tag (yaml_parser_scan_tag_uri). A blank, a
// quote, a `#`, a `{` and a `}` are in neither set — yaml.v3 refuses a
// property name carrying one. A `<` is in neither either: it opens the
// verbatim form `!<…>`, which propertyEnd delimits rather than read by
// class.
func propertyRune(r rune, tag bool) bool {
	switch {
	case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_', r == '-':
		return true
	case !tag:
		return false
	}
	return strings.ContainsRune(";/?:@&=+$,.!~*'()[]%", r)
}

// endsPlain reports whether the rune at ci ends a plain scalar inside a
// flow collection, as yaml.v3's scanner reads it: a flow indicator, `?`, or
// a `:` followed by a blank or the end of its line.
func endsPlain(line []rune, ci int) bool {
	switch line[ci] {
	case ',', '?', '[', ']', '{', '}':
		return true
	case ':':
		return ci+1 == len(line) || line[ci+1] == ' ' || line[ci+1] == '\t'
	}
	return false
}
