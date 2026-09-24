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
			out = append(out, flowComments(lines, n)...)
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
	return out
}

// flowComments reads the comments inside flow collection n off the source
// lines, in order, from its opening bracket to the one that closes it, as
// yaml.v3's scanner reads them: a `#` outside a quoted scalar starts a
// comment, to the end of its line, where a token may start — at the start
// of a line, after a blank, a flow indicator or a closing quote; inside a
// plain scalar (`a#b`) it is text. A quote opens a scalar only where a
// scalar starts (after a bracket, a `,`, a `:` or an explicit key's `?`);
// inside a plain one (`it's`) it is text.
func flowComments(lines []string, n *yaml.Node) []string {
	var out []string
	depth, started := 0, false
	var quote, prev rune // prev: the last rune outside a quote, blanks aside
	for li := n.Line - 1; li >= 0 && li < len(lines); li++ {
		line := []rune(lines[li])
		ci := 0
		if li == n.Line-1 {
			ci = max(n.Column-1, 0)
		}
		closedAt := -1 // where a quoted scalar closed on this line
		for ; ci < len(line); ci++ {
			r := line[ci]
			switch {
			case quote == '"':
				switch r {
				case '\\':
					ci++
				case '"':
					quote, prev, closedAt = 0, r, ci
				}
				continue
			case quote == '\'':
				if r == '\'' {
					if ci+1 < len(line) && line[ci+1] == '\'' {
						ci++
					} else {
						quote, prev, closedAt = 0, r, ci
					}
				}
				continue
			case !started:
				if r == '[' || r == '{' {
					started, depth, prev = true, 1, r
				}
				continue
			case (r == '"' || r == '\'') && strings.ContainsRune("[{,:", prev):
				quote = r
			case r == '[' || r == '{':
				depth++
			case r == ']' || r == '}':
				depth--
				if depth == 0 {
					return out
				}
			case r == '#' && (strings.TrimSpace(string(line[:ci])) == "" || strings.ContainsRune(" \t[]{},", line[ci-1]) || closedAt == ci-1):
				// yaml.v3's scanner reads a `#` where a token may start as a
				// comment — after a blank, a flow indicator or a closing
				// quote; only inside a plain scalar is it text (`a#b`).
				out = append(out, strings.TrimSpace(string(line[ci:])))
				ci = len(line)
				continue
			case r == '?' && (ci+1 == len(line) || line[ci+1] == ' ' || line[ci+1] == '\t'):
				// An explicit key's indicator: a scalar starts after it.
				prev = ','
				continue
			}
			if r != ' ' && r != '\t' {
				prev = r
			}
		}
	}
	return out
}
