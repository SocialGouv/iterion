package author

import (
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
		add(n.HeadComment)
		if n.Style&yaml.FlowStyle != 0 && (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) {
			before, inside := flowComments(lines, n)
			// Between a tag and its bracket, yaml.v3 hangs a comment of its
			// own line on the collection's head (listed above) and drops one
			// on the tag's line: only the dropped ones are added here — a
			// comment on the tag's line is never the head's.
			head := map[string]int{}
			for _, h := range strings.Split(n.HeadComment, "\n") {
				head[strings.TrimSpace(h)]++
			}
			for _, c := range before {
				if c.ownLine && head[c.text] > 0 {
					head[c.text]--
					continue
				}
				out = append(out, c.text)
			}
			out = append(out, inside...)
			add(n.LineComment)
			add(n.FootComment)
			return
		}
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
// with whether it is written on its own line.
func flowComments(lines []string, n *yaml.Node) (before []flowComment, out []string) {
	depth, started, plain := 0, false, false
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
					})
					ci = len(line)
				} else if r == '[' || r == '{' {
					started, depth = true, 1
				}
				continue
			case r == ' ' || r == '\t':
				continue
			case r == '#' && (!plain || ci == 0 || line[ci-1] == ' ' || line[ci-1] == '\t'):
				out = append(out, strings.TrimSpace(string(line[ci:])))
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
					return before, out
				}
			case ',', ':', '?':
			default:
				plain = true
			}
		}
	}
	return before, out
}

// flowComment is a comment read before a flow collection's bracket.
type flowComment struct {
	text    string
	ownLine bool // nothing but blanks before it on its line
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
