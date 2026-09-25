package author

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The bounds of a document the converter reads, the twins of the `.bot`
// lexer's own (10 MiB of source, 100 levels of nesting): a YAML file is not
// a way around them. The node count bounds the work a small file can ask
// for (a billion-laughs document has few bytes and many nodes) — anchors
// and aliases are refused outright, so the count is the honest one.
const (
	maxSourceSize   = 10 * 1024 * 1024
	maxNestingDepth = 100
	maxNodes        = 1 << 20
)

// yamlLineRe reads the line number yaml.v3 writes into its own errors
// ("yaml: line 3: mapping values are not allowed in this context").
var yamlLineRe = regexp.MustCompile(`line (\d+):`)

// decode reads exactly one YAML document off src and returns its root
// node, or the diagnostics that refuse it: a source too large, text that
// is not YAML, no document at all, a second document (a `---` separator
// among them), and — on the tree — every construct guardTree refuses.
func decode(name string, src []byte) (*yaml.Node, []parser.Diagnostic) {
	if len(src) > maxSourceSize {
		return nil, []parser.Diagnostic{docDiag(name, 1, 1, fmt.Sprintf("the document exceeds the maximum size (%d bytes > %d)", len(src), maxSourceSize))}
	}
	// yaml.v3 also reads UTF-16; every reader of the source at a node's
	// position reads UTF-8 (sourceLines), as the .bot it stands for is.
	if at := invalidUTF8(src); at >= 0 {
		return nil, []parser.Diagnostic{docDiag(name, len(sourceLines(src[:at])), 1, fmt.Sprintf("the document is not UTF-8 text (byte %d): write it in UTF-8, as the .bot it stands for is", at))}
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, []parser.Diagnostic{docDiag(name, 1, 1, "the document is empty: nothing to read")}
		}
		line := 1
		if m := yamlLineRe.FindStringSubmatch(err.Error()); m != nil {
			line, _ = strconv.Atoi(m[1])
		}
		return nil, []parser.Diagnostic{docDiag(name, line, 1, "not YAML: "+err.Error()+yamlRemedy(err.Error()))}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, []parser.Diagnostic{docDiag(name, 1, 1, "the document is empty: nothing to read")}
	}
	var second yaml.Node
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		line := second.Line
		if len(second.Content) > 0 && second.Content[0].Line > 0 {
			line = second.Content[0].Line
		}
		if line == 0 {
			line = 1
		}
		msg := "one document per file: a second document follows"
		if err != nil {
			msg = "one document per file: a second document follows, and it is not YAML: " + err.Error()
		}
		return nil, []parser.Diagnostic{docDiag(name, line, 1, msg)}
	}
	root := doc.Content[0]
	diags := guardTree(name, root, src)
	if len(diags) > 0 {
		return nil, diags
	}
	return root, nil
}

// yamlRemedy names, for the YAML syntax errors an author of a .bot twin
// meets first, the spelling that avoids them — yaml.v3's own message says
// what the scanner saw, not what to write. Measured on the authoring probe:
// a shell command holding a `: ` (a jq filter) written unquoted cost a
// mid-size model four validation rounds, the message repeating itself. A
// message the scanner gives for two different mistakes names both: the
// line it reports does not tell them apart (a quoted value that ends early
// is reported at the start of its mapping).
func yamlRemedy(msg string) string {
	switch {
	case strings.Contains(msg, "mapping values are not allowed"):
		return " — a plain value holding a `:` followed by a space or by the end of the line reads as a nested mapping: quote the whole value (a command with a jq filter, an expression, an edge whose with map has a `: `, a text ending in `:`), a `'` inside single quotes written twice (`''`); or a key is indented deeper than the keys beside it"
	case strings.Contains(msg, "cannot start any token"):
		return " — the value starts with a character YAML reserves (`%`, `@`, a backtick): quote the whole value; or a tab indents the line, where YAML takes spaces only"
	case strings.Contains(msg, "found a tab character"):
		return " — YAML indents with spaces only: replace the tab"
	case strings.Contains(msg, "did not find expected key"), strings.Contains(msg, "did not find expected '-' indicator"):
		return " — a value that starts with `{` or `[` (a `{{…}}` template, a `[WIP]`) is read by YAML as a mapping or a list, and a value with quotes of its own, written plain or inside double quotes, ends where YAML reads its quote as closed (an edge whose with map holds a quoted value): write either whole in single quotes, its double quotes as they are and a `'` inside written twice (`''`); or indentation: the keys of one mapping start at one column, a value's lines are indented under its key, a list's items under theirs"
	case strings.Contains(msg, "could not find expected ':'"):
		return " — a key is followed by `: ` (colon, space) and its value"
	case strings.Contains(msg, "found unexpected end of stream"), strings.Contains(msg, "found unexpected document indicator"):
		return " — a quoted value or a block was left open, or a `---` line sits inside the document: one document, every quote closed"
	}
	return ""
}

// guardTree walks the tree once and refuses what the converter never
// reads: an alias or an anchor (a value is written where it is used), an
// explicit tag (the value's shape decides its type; a `!!str 3` would say
// otherwise) — the non-specific tag `!` of a scalar included, which yaml.v3
// drops without marking the node (read off src: a `!` where the scalar
// starts) — a merge key, a key that is not a plain string, a key
// repeated in one mapping (yaml.v3 keeps both; the .bot has one), a tree
// deeper or larger than the lexer's bounds.
func guardTree(name string, root *yaml.Node, src []byte) []parser.Diagnostic {
	g := &guard{name: name, lines: sourceLines(src)}
	g.walk(root, 0)
	return g.diags
}

// sourceLines cuts src into the lines yaml.v3's positions count: a UTF-8
// BOM off, as the scanner skips it, and a line ended where the scanner ends
// one (yamlBreaks) — every reader of the source at a node's position reads
// it through here.
func sourceLines(src []byte) []string {
	return strings.Split(yamlBreaks.Replace(strings.TrimPrefix(string(src), "\ufeff")), "\n")
}

// invalidUTF8 is the offset of src's first byte that is not UTF-8, -1 when
// every byte is.
func invalidUTF8(src []byte) int {
	for i := 0; i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return -1
}

type guard struct {
	name  string
	lines []string
	count int
	diags []parser.Diagnostic
}

// at is the source from node n's position to the end of its line, "" when
// the position is not on a line of the source.
func (g *guard) at(n *yaml.Node) string {
	if n.Line < 1 || n.Line > len(g.lines) {
		return ""
	}
	line := []rune(g.lines[n.Line-1])
	if col := n.Column - 1; col >= 0 && col < len(line) {
		return string(line[col:])
	}
	return ""
}

// droppedBang reports whether YAML read a `!` at scalar n's position as a
// tag and dropped it, without a mark on the node — yaml.v3 marks a named
// tag, never the non-specific `!`. A scalar of any style: a quoted value
// starts with its quote, a block with `|` or `>`, a plain value never with
// `!` — so a `!` where the node starts is its tag. `! grep -q x f` would
// otherwise reach the .bot as `grep -q x f`, `! 'test -f x'` as the command
// it negates. (A collection's `!` changes nothing: its kind is its type.)
func (g *guard) droppedBang(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Style&yaml.TaggedStyle == 0 && strings.HasPrefix(g.at(n), "!")
}

// excerpt is v cut short for a message: its first line, 60 characters at most.
func excerpt(v string) string {
	v, _, _ = strings.Cut(v, "\n")
	if r := []rune(v); len(r) > 60 {
		return string(r[:60]) + "\u2026"
	}
	return v
}

func (g *guard) refuse(n *yaml.Node, msg string) {
	g.diags = append(g.diags, docDiag(g.name, n.Line, n.Column, msg))
}

func (g *guard) walk(n *yaml.Node, depth int) {
	if n == nil {
		return
	}
	g.count++
	if g.count > maxNodes {
		if g.count == maxNodes+1 {
			g.refuse(n, fmt.Sprintf("the document holds more than %d values", maxNodes))
		}
		return
	}
	if depth > maxNestingDepth {
		g.refuse(n, fmt.Sprintf("maximum nesting depth exceeded (%d levels)", maxNestingDepth))
		return
	}
	if n.Anchor != "" {
		g.refuse(n, "an anchor (&"+n.Anchor+") is not read: write the value where it is used")
	}
	if n.Style&yaml.TaggedStyle != 0 {
		g.refuse(n, "an explicit tag ("+n.Tag+") is not read: the value's shape decides its type")
	}
	if g.droppedBang(n) {
		g.refuse(n, "YAML reads a `!` before a value as a tag and drops it — `"+excerpt(g.at(n))+"` would be read without it: quote the whole value if the `!` is part of it")
	}
	switch n.Kind {
	case yaml.AliasNode:
		g.refuse(n, "an alias (*"+n.Value+") is not read: write the value where it is used")
		return
	case yaml.MappingNode:
		seen := map[string]int{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			switch {
			case k.Kind == yaml.MappingNode && strings.HasPrefix(g.at(n), "{{"):
				// `user: {{input.task}}` reads as a mapping whose key is a
				// mapping: a template written unquoted.
				g.refuse(k, "a key is a plain word, not a mapping — a value that starts with `{{` (a template) is read by YAML as a mapping: quote the whole value")
			case k.Kind != yaml.ScalarNode:
				g.refuse(k, "a key is a plain word, not a "+kindWord(k))
			case k.Tag == "!!merge": // a plain `<<`; quoted, a key like any other
				g.refuse(k, "a merge key (<<) is not read: write the entries in place")
			case k.ShortTag() != "!!str":
				g.refuse(k, "a key is a plain word; `"+k.Value+"` reads as "+tagWord(k.ShortTag())+" — quote it if it is a name")
			default:
				if first, dup := seen[k.Value]; dup {
					g.refuse(k, fmt.Sprintf("the key `%s` appears twice in this mapping (first at line %d): a key names its value once", k.Value, first))
				} else {
					seen[k.Value] = k.Line
				}
			}
			g.walk(k, depth+1)
			g.walk(v, depth+1)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			g.walk(c, depth+1)
		}
	case yaml.DocumentNode:
		for _, c := range n.Content {
			g.walk(c, depth)
		}
	}
}

func docDiag(name string, line, col int, msg string) parser.Diagnostic {
	if line < 1 {
		line = 1
	}
	if col < 1 {
		col = 1
	}
	return parser.Diagnostic{
		Code: parser.DiagAuthorDocument, Severity: parser.SeverityError,
		Message: msg, File: name, Line: line, Column: col,
		Hint: parser.HintFor(parser.DiagAuthorDocument),
	}
}

// kindWord names a node's kind for a message.
func kindWord(n *yaml.Node) string {
	if n == nil {
		return "nothing"
	}
	switch n.Kind {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "list"
	case yaml.AliasNode:
		return "alias"
	case yaml.ScalarNode:
		return tagWord(n.ShortTag())
	}
	return "value"
}

// tagWord names a resolved scalar tag for a message: what the author's
// spelling READS as, which is what surprises them.
func tagWord(tag string) string {
	switch tag {
	case "!!str":
		return "a string"
	case "!!int":
		return "an integer"
	case "!!float":
		return "a float"
	case "!!bool":
		return "a bool"
	case "!!null":
		return "null (no value)"
	case "!!timestamp":
		return "a timestamp (a bare date)"
	case "!!binary":
		return "binary data"
	case "!!merge":
		return "YAML's merge key"
	}
	return tag
}
