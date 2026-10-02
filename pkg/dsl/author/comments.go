package author

import (
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
// The writer keeps none of them (Write renders the program, not the
// document), so a rewrite of a document that carries one would lose it:
// `iterion fmt` asks here before it rewrites a document in its canonical
// form, and refuses when the answer is not empty; `fmt --to bot` counts
// them and names the first. A source that is not YAML has no comments to
// report.
func Comments(src []byte) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil
	}
	lines := sourceLines(src)
	var out []string
	add := func(block string) {
		for _, line := range strings.Split(block, "\n") {
			if strings.TrimSpace(line) != "" {
				out = append(out, strings.TrimSpace(line))
			}
		}
	}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Style&yaml.FlowStyle != 0 && (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) {
			out = append(out, walkFlow(n, lines)...)
			return
		}
		add(n.HeadComment)
		add(n.LineComment)
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				add(k.HeadComment)
				add(k.LineComment)
				for _, c := range k.Content {
					walk(c)
				}
				walk(n.Content[i+1])
				add(k.FootComment)
			}
		} else {
			for _, c := range n.Content {
				walk(c)
			}
		}
		add(n.FootComment)
	}
	walk(&doc)
	// A comment on a directive's line (`%YAML 1.1 # c`, `%TAG …`) is no
	// node's: yaml.v3 drops it. It is read off the source and listed after
	// the own-line comments written above the first directive, which
	// yaml.v3 lists first, on the document's head.
	if above, dc := directiveComments(lines); len(dc) > 0 {
		at := min(above, len(out))
		out = append(out[:at], append(dc, out[at:]...)...)
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
//     is the moved inside comment (the `[a: # c` + break shapes) — UNLESS
//     a copy of it also sits at or after the CLOSING bracket's line, where
//     it is the author's own separate comment, listed;
//   - anything else — a comment between a `!` tag and its bracket the head
//     never carried, a same-text comment on another line — is listed, same
//     text or not (the one-short fuzz case).
//
// A head whose block cannot be found above the opening bracket is still
// listed, from the hung text: failing to find it must not lose it.
func walkFlow(n *yaml.Node, lines []string) []string {
	before, inside, openLine, closeLine := flowComments(lines, n)
	headLines := headCommentLines(lines, openLine, n.HeadComment)
	sourced := append(append([]flowComment{}, before...), inside...)
	sort.Slice(sourced, func(i, j int) bool { return sourced[i].line < sourced[j].line })
	var out []string
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
		add := func(block string) {
			for _, line := range strings.Split(block, "\n") {
				if strings.TrimSpace(line) != "" {
					out = append(out, strings.TrimSpace(line))
				}
			}
		}
		add(n.HeadComment)
	}
	sourcedText := map[string]bool{}
	for _, c := range sourced {
		if headLines[c.line] {
			continue
		}
		out = append(out, c.text)
		sourcedText[c.text] = true
	}
	for _, hung := range []string{n.LineComment, n.FootComment} {
		for _, l := range strings.Split(hung, "\n") {
			t := strings.TrimSpace(l)
			if t == "" {
				continue
			}
			if sourcedText[t] && !commentAfterClose(t, lines, closeLine) {
				continue
			}
			out = append(out, t)
		}
	}
	return out
}

// commentAfterClose reports whether a comment of this text is written on a
// line at or after the closing bracket's own — a physical comment the
// sourced read (which stops at that bracket) never saw. A same-text
// comment with no such line is the inside comment yaml.v3 moved onto the
// node's line or foot (#1814).
func commentAfterClose(text string, lines []string, closeLine int) bool {
	for li := closeLine - 1; li < len(lines); li++ {
		r := []rune(lines[li])
		for ci := 0; ci+1 < len(r); ci++ {
			if r[ci] == '#' && (ci == 0 || r[ci-1] == ' ' || r[ci-1] == '\t') {
				return strings.TrimSpace(string(r[ci:])) == text
			}
		}
	}
	return false
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
			r := []rune(l)
			for ci := 1; ci < len(r); ci++ {
				if r[ci] == '#' && (r[ci-1] == ' ' || r[ci-1] == '\t') {
					comments = append(comments, strings.TrimSpace(string(r[ci:])))
					break
				}
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
// comment to the end of its line, a quote opens a quoted scalar, `:` and
// `?` are indicators whatever follows them (in a flow collection), and any
// other character starts a plain scalar. Inside a plain scalar a quote is
// text (`it's`, `b:'c`), and so is a `#` with no blank before it (`a#b`);
// the scalar ends at `,`, `?`, a bracket, or a `:` followed by a blank or
// the end of its line, and goes on across blanks and line breaks otherwise.
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
				// (yaml.v3 places a tagged collection there), and a `#`
				// there starts a comment, a bracket inside it included.
				if r == '#' && (ci == 0 || line[ci-1] == ' ' || line[ci-1] == '\t') {
					before = append(before, flowComment{
						text:    strings.TrimSpace(string(line[ci:])),
						ownLine: strings.TrimSpace(string(line[:ci])) == "",
						line:    li + 1,
					})
					ci = len(line)
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
