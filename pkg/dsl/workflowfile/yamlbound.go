package workflowfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	yamlv3 "gopkg.in/yaml.v3"
)

// The bound a decode of an author's YAML is held to before it runs: the
// author document's (pkg/dsl/author — its reading, its comments, its
// rewrite), `iterion fmt`'s readings of it, and a `.bot` frontmatter
// block's (DecodeFrontmatter). A YAML reader builds a document's whole
// tree before any caller sees a node and offers no hook to count while it
// builds, so the decode's cost is bounded off the text, before the decode:
// its nodes cost it some 200 bytes of memory each (measured on yaml.v3, a
// flow mapping of one-letter keys being the densest text, a node a byte),
// the text of a value a few bytes a byte, whatever it spells.
//
// The values a text can open are counted off its indicators
// (YAMLValuesBound) — over, never under. A text a reader holds as one value
// — the text of a `|` or `>` block, which an author's prompt, a JSON
// example in it included, is written in — is no structure, and counting it
// would refuse a document for the punctuation of its prose: where the count
// of the whole text passes the ceiling, the blocks' text is set aside and
// the rest counted (setAsideBlocks), on a skeleton of the text whose
// reading by yaml.v3 proves each block is one (blocksHold).

// YAMLValuesBound is the most values — nodes, the root and the document
// included — a YAML reader can build off src: every node is the root or is
// opened by an indicator, each counted for what it can open — a `:` a key,
// its value and the mapping, a `{` a flow mapping and its first pair, a `,`
// the next entry of a flow collection (a pair at most), a `?` an explicit
// key and its value, a `[` a flow sequence and its first entry, a `-` the
// entry of a block sequence — and the comments the reader keeps beside
// them, a `#` two: yaml.v3 holds every comment token it scans and hangs its
// text on a node, some 500 bytes of memory each against some 200 for a
// node, so one comment a node — `#` and `-` on lines of their own — would
// otherwise cost three times what the bound admits. Two is the weight the
// author reader's honest count gives a comment line (pastReadBudget), so
// the bound before the decode stays over the count after it. An indicator
// inside a value's text counts too: the bound is over, never under. Seven
// byte counts: none of these bytes occurs inside another UTF-8 character.
func YAMLValuesBound(src []byte) int {
	return 2 + 3*bytes.Count(src, []byte{':'}) + 3*bytes.Count(src, []byte{'{'}) +
		2*bytes.Count(src, []byte{','}) + 2*bytes.Count(src, []byte{'?'}) +
		2*bytes.Count(src, []byte{'['}) + bytes.Count(src, []byte{'-'}) +
		2*bytes.Count(src, []byte{'#'})
}

// yamlBoundPassed is the offset of the indicator at which src's running
// count (YAMLValuesBound) passes ceiling.
func yamlBoundPassed(src []byte, ceiling int) int {
	u := 2
	for i, b := range src {
		switch b {
		case ':', '{':
			u += 3
		case ',', '?', '[', '#':
			u += 2
		case '-':
			u++
		default:
			continue
		}
		if u > ceiling {
			return i
		}
	}
	return len(src) - 1
}

// YAMLDecodeRefusal is a YAML text refused before its decode runs
// (CheckYAMLDecode): its indicators could open more values than the
// ceiling it was held to.
type YAMLDecodeRefusal struct {
	// Line and Column place the indicator at which the count passed the
	// ceiling, as yaml.v3 places a node: lines as it ends them (LF, CRLF,
	// CR, NEL, LS, PS), columns in characters, a leading BOM aside.
	Line, Column int
	// Ceiling is the count the text was held to.
	Ceiling int
	// BlocksAside says the text of the `|` and `>` blocks was set aside and
	// the indicators outside it passed the ceiling; false, every indicator
	// of the text was counted.
	BlocksAside bool
}

// Counted says which indicators the refusal counted, for its message.
func (r *YAMLDecodeRefusal) Counted() string {
	if r.BlocksAside {
		return "each `:` `,` `?` `-` `[` `{` and `#` outside the text of its `|` and `>` blocks"
	}
	return "each `:` `,` `?` `-` `[` `{` and `#` it holds"
}

func (r *YAMLDecodeRefusal) Error() string {
	return fmt.Sprintf("line %d: the YAML may hold more than %d values — counted before it is read, %s as the values and comments it can open", r.Line, r.Ceiling, r.Counted())
}

// CheckYAMLDecode reports whether a YAML reader may decode src without
// building more than ceiling values: nil when it may, the refusal — placed
// where the count passed the ceiling — when its indicators could open more.
// setAsideBlocks sets the text of src's `|` and `>` blocks aside when the
// count of the whole text passes the ceiling; it is for a yaml.v3 reader
// alone, whose reading of the blocks proves the skeleton (blocksHold).
func CheckYAMLDecode(src []byte, ceiling int, setAsideBlocks bool) *YAMLDecodeRefusal {
	if YAMLValuesBound(src) <= ceiling {
		return nil
	}
	if setAsideBlocks {
		if skel, blocks := yamlSkeleton(src); len(blocks) > 0 {
			if YAMLValuesBound(skel) > ceiling {
				// The structure alone passes the ceiling: placed on it, a
				// line the skeleton holds as written.
				line, col := yamlPosition(skel, yamlBoundPassed(skel, ceiling))
				return &YAMLDecodeRefusal{Line: line, Column: col, Ceiling: ceiling, BlocksAside: true}
			}
			if blocksHold(skel, blocks) {
				return nil
			}
		}
	}
	line, col := yamlPosition(src, yamlBoundPassed(src, ceiling))
	return &YAMLDecodeRefusal{Line: line, Column: col, Ceiling: ceiling}
}

// yamlPosition is the line and column of byte at of src as yaml.v3 counts
// them (YAMLDecodeRefusal), read off the text with nothing materialised.
func yamlPosition(src []byte, at int) (line, col int) {
	start := 0
	if bytes.HasPrefix(src, []byte("\ufeff")) {
		start = len("\ufeff")
	}
	line, lineStart := 1, start
	for i := start; i < at && i < len(src); {
		if n := yamlBreakAt(src, i); n > 0 {
			line++
			i += n
			lineStart = i
			continue
		}
		i++
	}
	if at > len(src) {
		at = len(src)
	}
	if at < lineStart {
		at = lineStart
	}
	return line, utf8.RuneCount(src[lineStart:at]) + 1
}

// yamlBreakAt is the length of the line break yaml.v3's scanner reads at
// src[i] — CRLF, CR, LF, NEL, LS or PS (its is_break) — 0 for none.
func yamlBreakAt(src []byte, i int) int {
	switch c := src[i]; {
	case c == '\n':
		return 1
	case c == '\r':
		if i+1 < len(src) && src[i+1] == '\n' {
			return 2
		}
		return 1
	case c == 0xC2 && i+1 < len(src) && src[i+1] == 0x85:
		return 2
	case c == 0xE2 && i+2 < len(src) && src[i+1] == 0x80 && (src[i+2] == 0xA8 || src[i+2] == 0xA9):
		return 3
	}
	return 0
}

// textBlock is the text of a `|` or `>` block a skeleton sets aside: the
// line of its header (1-based, as yaml.v3 counts lines) and how many of
// its lines hold text.
type textBlock struct {
	header, lines int
}

// yamlSkeleton is src with the text of every `|` and `>` block it
// presumably holds set aside, and those blocks: each line of such a text
// keeps its indentation and the rest of it becomes one `x` — a blank line
// stays as it is — every other line is as written, every line break an LF,
// a leading BOM dropped. Lines and the positions on the lines kept are
// src's. "Presumably": a header is a line ending in the indicator
// (blockHeader), and its text the lines below it indented as yaml.v3 finds
// a block's indentation — past the header's key, by its digit or as deep as
// the first line of text — a guess about YAML that blocksHold proves or
// refuses, never trusted on its own.
func yamlSkeleton(src []byte) ([]byte, []textBlock) {
	// A first pass finds the blocks, writing nothing: a text with none
	// costs no copy of itself.
	if blocks := skeletonPass(src, nil); len(blocks) == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	out.Grow(len(src))
	blocks := skeletonPass(src, &out)
	return out.Bytes(), blocks
}

// skeletonPass reads src as yamlSkeleton says, writing the skeleton to out
// when it is not nil, and returns the blocks set aside.
func skeletonPass(src []byte, out *bytes.Buffer) []textBlock {
	src = bytes.TrimPrefix(src, []byte("\ufeff"))
	write := func(b []byte) {
		if out != nil {
			out.Write(b)
		}
	}
	var blocks []textBlock
	// parent is the column a block's text is indented past and indent the
	// one yaml.v3 reads it at: the header's digit past parent, or else the
	// first line of text's — 0 until that line is met.
	in, parent, indent, line := false, 0, 0, 0
	for start := 0; ; {
		end, next := start, -1
		for end < len(src) {
			if n := yamlBreakAt(src, end); n > 0 {
				next = end + n
				break
			}
			end++
		}
		text := src[start:end]
		line++
		ws := 0
		for ws < len(text) && (text[ws] == ' ' || text[ws] == '\t') {
			ws++
		}
		spaces := 0
		for spaces < len(text) && text[spaces] == ' ' {
			spaces++
		}
		blank := ws == len(text)
		if in && !blank && indent == 0 && spaces > parent {
			indent = spaces
		}
		switch {
		case in && blank:
			write(text)
		case in && indent > 0 && spaces >= indent:
			write(text[:ws])
			write([]byte{'x'})
			blocks[len(blocks)-1].lines++
		default:
			in = false
			write(text)
			if p, digit, ok := blockHeader(text); ok {
				in, parent, indent = true, p, 0
				if digit > 0 {
					indent = p + digit
				}
				blocks = append(blocks, textBlock{header: line})
			}
		}
		if next < 0 {
			break
		}
		write([]byte{'\n'})
		start = next
	}
	kept := blocks[:0]
	for _, b := range blocks {
		if b.lines > 0 {
			kept = append(kept, b)
		}
	}
	return kept
}

// blockHeader reports whether a line presumably opens a `|` or `>` block —
// it ends in the indicator, standing alone or after a space, with at most
// two of its chomping and indentation characters, and then at most a
// comment (` # …`, or `#…` right after them, which yaml.v3 scans as one
// there too) — the column its text
// must be indented past — the column of the key the block is the value of,
// or of the `-` or `?` entry it is, past the `- ` and `? ` the line opens
// with — and the indentation digit, 0 for none. A line indented with a tab
// is not taken for one: its text is counted. Neither is a body line
// indented with a tab before its spaces, which ends the block in the
// skeleton as yaml.v3 ends it — a tab is no indentation there, and yaml.v3
// refuses the line after it — so a text holding one is counted whole: a
// refusal by the count, on a document yaml.v3 would refuse anyway.
func blockHeader(text []byte) (parent, digit int, ok bool) {
	if parent, digit, ok = blockHeaderBare(text); ok {
		return parent, digit, true
	}
	// A comment after the indicator: the line before its `#`, read as a
	// header on its own. Each `#` is tried — after a blank, or right after
	// the indicators, where yaml.v3 opens a comment too (`|#note`) — the
	// first that leaves a header winning: a `#` inside the comment's own
	// text, or one inside a key or a value, leaves none before it, or one
	// blocksHold disproves. The guess is blocksHold's to prove.
	for i := 1; i < len(text); i++ {
		if text[i] == '#' {
			if parent, digit, ok = blockHeaderBare(text[:i]); ok {
				return parent, digit, true
			}
		}
	}
	return 0, 0, false
}

// blockHeaderBare is blockHeader for a line that carries no comment.
func blockHeaderBare(text []byte) (parent, digit int, ok bool) {
	t := bytes.TrimRight(text, " \t")
	i := len(t)
	for k := 0; k < 2 && i > 0 && (t[i-1] == '+' || t[i-1] == '-' || t[i-1] >= '1' && t[i-1] <= '9'); k++ {
		if t[i-1] >= '1' && t[i-1] <= '9' {
			digit = int(t[i-1] - '0')
		}
		i--
	}
	if i == 0 || t[i-1] != '|' && t[i-1] != '>' || i >= 2 && t[i-2] != ' ' {
		return 0, 0, false
	}
	col := 0
	for col < len(text) && text[col] == ' ' {
		col++
	}
	if col < len(text) && text[col] == '\t' {
		return 0, 0, false
	}
	// Past each `- ` or `? ` entry the line opens with, the next column is
	// the entry's node: a key there is what the block's text is indented
	// past, the last entry's own column when the block is the entry.
	parent = col
	for at := i - 1; col+1 < at && (text[col] == '-' || text[col] == '?') && text[col+1] == ' '; {
		parent = col
		col += 2
		for col < at && text[col] == ' ' {
			col++
		}
		if col < at {
			parent = col
		}
	}
	return parent, digit, true
}

// blocksHold reports whether yaml.v3 reads every block a skeleton set aside
// as one: a `|` or `>` scalar opening on the block's header line whose text
// holds the block's every line — one `x` each, the skeleton's — and so no
// line of it as a value of its own. A block scalar's reach is a matter of
// indentation, blank lines and line breaks alone, which the skeleton keeps
// line for line: yaml.v3 then reads the text the skeleton replaced as the
// same one value, and src builds no more values than the skeleton, whose
// count the ceiling held. A skeleton yaml.v3 does not read, or reads
// otherwise, proves nothing: the text is counted.
func blocksHold(skel []byte, blocks []textBlock) bool {
	texts := map[int]int{}
	var walk func(n *yamlv3.Node)
	walk = func(n *yamlv3.Node) {
		if n.Kind == yamlv3.ScalarNode && n.Style&(yamlv3.LiteralStyle|yamlv3.FoldedStyle) != 0 {
			texts[n.Line] = bytes.Count([]byte(n.Value), []byte{'x'})
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	dec := yamlv3.NewDecoder(bytes.NewReader(skel))
	for {
		var doc yamlv3.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false
		}
		walk(&doc)
	}
	for _, b := range blocks {
		// The count of x's, beside the header's line: yaml.v3 takes a
		// block's indentation from the deepest of its LEADING BLANK lines
		// when it has no digit (max_indent, vendor/gopkg.in/yaml.v3
		// scannerc.go yaml_parser_scan_block_scalar_breaks), where the
		// skeleton takes the first line of text's — a header could then
		// open an empty scalar on its line while the text below it is read
		// as structure. No valid text built against the two rules tells
		// them apart (each one tried, a fuzzing of millions: a text the
		// skeleton sets aside there is one yaml.v3 refuses to read), which
		// is no proof that none does, nor that a later yaml.v3 keeps the
		// rule: the count is what holds the guard if one ever does.
		if n, ok := texts[b.header]; !ok || n != b.lines {
			return false
		}
	}
	return true
}
