package author

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

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
	diags := guardTree(name, root)
	if len(diags) > 0 {
		return nil, diags
	}
	return root, nil
}

// yamlRemedy names, for the YAML syntax errors an author of a .bot twin
// meets first, the spelling that avoids them — yaml.v3's own message says
// what the scanner saw, not what to write. Measured on the authoring probe:
// a shell command holding a `: ` (a jq filter) written unquoted cost a
// mid-size model four validation rounds, the message repeating itself.
func yamlRemedy(msg string) string {
	switch {
	case strings.Contains(msg, "mapping values are not allowed"):
		return " — a plain value holding `: ` reads as a nested mapping: quote the whole value (a command with a jq filter, an expression, an edge whose with map has a `: `)"
	case strings.Contains(msg, "cannot start any token"):
		return " — the value starts with a character YAML reserves (`%`, `@`, a backtick, `|`, `>`, `*`, `&`, `!`, `[`, `{`): quote the whole value"
	case strings.Contains(msg, "did not find expected key"), strings.Contains(msg, "did not find expected '-' indicator"):
		return " — indentation: the keys of one mapping start at one column, a value's lines are indented under its key, a list's items under theirs"
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
// otherwise), a merge key, a key that is not a plain string, a key
// repeated in one mapping (yaml.v3 keeps both; the .bot has one), a tree
// deeper or larger than the lexer's bounds.
func guardTree(name string, root *yaml.Node) []parser.Diagnostic {
	g := &guard{name: name}
	g.walk(root, 0)
	return g.diags
}

type guard struct {
	name  string
	count int
	diags []parser.Diagnostic
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
	switch n.Kind {
	case yaml.AliasNode:
		g.refuse(n, "an alias (*"+n.Value+") is not read: write the value where it is used")
		return
	case yaml.MappingNode:
		seen := map[string]int{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			switch {
			case k.Kind != yaml.ScalarNode:
				g.refuse(k, "a key is a plain word, not a "+kindWord(k))
			case k.Tag == "!!merge" || k.Value == "<<":
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
	}
	return tag
}
