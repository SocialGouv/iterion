package unparse

import (
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Comment placement — putting the `##` lines back where they were written.
//
// The writers render the program; this pass renders what was written AROUND
// it. It reads the text they produced with the same walk the parser reads
// the source with (parser.Scan), so "the line named `sandbox.network`" means
// the same thing on both sides, and a comment goes back above the line it
// led. The text is the writer's own, already a `.bot`: the walk lexes it, so
// a `#` inside a string or a prompt body is text here exactly as it is
// there, and no line is ever found by looking at its characters.
//
// A comment whose anchor the document no longer has — the property it led
// was cleared on the canvas — is written at the end of the block it was in,
// never dropped. Unparse then proves the result (Verify): a placement that
// changed the program is refused by name rather than written.

// placeComments returns text with every comment f's declarations carry
// written back at its address, and the file's own tail comments appended.
// A document carrying none gets its text back untouched.
func placeComments(name, text string, f *ast.File) string {
	carriers := ast.CommentCarriers(f)
	tail := fileComments(f.Comments, ast.CommentAtEnd)
	if !ast.HasPlacedComments(f) && len(tail) == 0 {
		return text
	}
	lines, _ := parser.ScanSource(name, text)
	idx := newTextIndex(lines)
	ed := &editor{lines: strings.Split(text, "\n")}
	firstCode := 0
	if len(lines) > 0 {
		firstCode = lines[0].Line
	}

	for i, c := range carriers {
		d := idx.decl(c.Kind, c.Name)
		ed.placeRun(d, *c.Comments, "")
		if i == 0 && len(fileHeadComments(f.Comments)) == 0 && d != nil && d.header.Line == firstCode {
			// This declaration opens the file and nothing is written
			// above it: its leading comments then stand where the file's
			// head stands, and the next read calls them the head's. Close
			// them with the blank line writeHead would have written, so
			// the text the next read produces is this one.
			ed.closeHead(d.header.Line)
		}
		for j, e := range c.Edges {
			if e == nil {
				continue
			}
			ed.placeRun(d, e.Comments, parser.EdgeKey(j+1))
		}
	}
	for _, cm := range tail {
		ed.appendTail(cm)
	}
	return ed.render()
}

// fileComments are the file's own comments sitting at one place: its tail
// (ast.CommentAtEnd), appended here.
func fileComments(comments []*ast.Comment, place ast.CommentPlace) []*ast.Comment {
	var out []*ast.Comment
	for _, c := range comments {
		if c != nil && c.Place == place {
			out = append(out, c)
		}
	}
	return out
}

// fileHeadComments are the file's head: everything it carries that is not
// its tail. A `trailing` place has no meaning on the file itself — there is
// no line of its own to end — and the transport takes one all the same, so
// it is written at the head rather than dropped.
func fileHeadComments(comments []*ast.Comment) []*ast.Comment {
	var out []*ast.Comment
	for _, c := range comments {
		if c != nil && c.Place != ast.CommentAtEnd {
			out = append(out, c)
		}
	}
	return out
}

// textIndex is the address of every code line of one rendered text, grouped
// by the declaration it belongs to.
type textIndex struct {
	decls map[declKey]*textDecl
}

type declKey struct{ kind, name string }

// textDecl is one declaration of the rendered text: its header line, the
// line each of its paths opens on, and the extent of its body.
type textDecl struct {
	header parser.CodeLine
	byPath map[string]parser.CodeLine
	edges  map[int]parser.CodeLine
	// last is the line its body ends on, bodyCol the column its body is
	// written at (0 when it has none).
	last    int
	bodyCol int
	// blockEnd is, per path, the line the block that path opens ends on,
	// and blockCol the column of that block's own lines.
	blockEnd map[string]int
	blockCol map[string]int
}

func newTextIndex(lines []parser.CodeLine) *textIndex {
	idx := &textIndex{decls: map[declKey]*textDecl{}}
	for _, l := range lines {
		if l.Decl.IsFile() {
			continue
		}
		k := declKey{l.Decl.Kind, l.Decl.Name}
		d := idx.decls[k]
		if d == nil {
			d = &textDecl{byPath: map[string]parser.CodeLine{}, edges: map[int]parser.CodeLine{}, blockEnd: map[string]int{}, blockCol: map[string]int{}}
			idx.decls[k] = d
		}
		if l.Path == "" && l.Keyed {
			d.header = l
			continue
		}
		if d.last < l.End {
			d.last = l.End
		}
		if l.Col > d.header.Col && (d.bodyCol == 0 || l.Col < d.bodyCol) {
			// A line at or left of the header's own column is not the
			// body's indentation — the closing brace of an edge's or a
			// `use`'s `with { … }` is written at column 1, and taking it
			// put a comment meant for the end of the declaration at the
			// left margin, where the next read gives it to whatever
			// declaration follows.
			d.bodyCol = l.Col
		}
		if l.Edge > 0 {
			d.edges[l.Edge] = l
		}
		if l.Keyed {
			if _, seen := d.byPath[l.Path]; !seen {
				d.byPath[l.Path] = l
			}
		}
		if !l.Keyed && l.Path != "" {
			// An unkeyed line names the block it is IN, not one of its
			// own: it is that block's content, and the block ends at
			// least here.
			if d.blockEnd[l.Path] < l.End {
				d.blockEnd[l.Path] = l.End
			}
			if d.blockCol[l.Path] == 0 || l.Col < d.blockCol[l.Path] {
				d.blockCol[l.Path] = l.Col
			}
		}
		// Every block this line lies in ends at least here.
		for _, p := range ancestors(l.Path) {
			if d.blockEnd[p] < l.End {
				d.blockEnd[p] = l.End
			}
			if d.blockCol[p] == 0 || l.Col < d.blockCol[p] {
				d.blockCol[p] = l.Col
			}
		}
	}
	return idx
}

// ancestors are the paths of the blocks a path lies inside, itself
// excluded: "a.b.c" lies in "a.b" and in "a".
func ancestors(path string) []string {
	if path == "" {
		return nil
	}
	parts := strings.Split(path, ".")
	out := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		out = append(out, strings.Join(parts[:i], "."))
	}
	return out
}

func (idx *textIndex) decl(kind, name string) *textDecl {
	return idx.decls[declKey{kind, name}]
}

// editor accumulates the comment lines to insert into a rendered text and
// renders it once.
type editor struct {
	lines  []string
	before map[int][]string
	after  map[int][]string
	trail  map[int]string
	tail   []string
}

// placeRun writes a declaration's comments. A paragraph break is honoured
// between two comments of the SAME address and never before the first of
// them: what separates a run from the code above it is the writer's own
// blank line, and a break there would double it.
func (e *editor) placeRun(d *textDecl, comments []*ast.Comment, anchor string) {
	opened := map[string]bool{}
	for _, cm := range comments {
		if cm == nil {
			continue
		}
		if parser.IsStrictEscapeDirective(cm.Text) {
			// The file's mode, not a note: writeHead places it — once,
			// at the head, where the lexer reads it — and profile 2
			// replaces it with the `dsl:` header. Writing it here too
			// gave the file a second copy on every rewrite.
			continue
		}
		at := anchor
		if at == "" {
			at = cm.Anchor
		}
		key := at + "\x00" + strconv.Itoa(int(cm.Place))
		e.place(d, cm, at, cm.Blank && opened[key])
		opened[key] = true
	}
}

// place writes one comment at the address it carries, under the
// declaration d of the rendered text.
func (e *editor) place(d *textDecl, c *ast.Comment, anchor string, blank bool) {
	if c == nil {
		return
	}
	if d == nil {
		// The writer has no such declaration — a document edited between
		// the parse and the save. The comment keeps its text at the end
		// of the file rather than disappear with the address.
		e.tail = append(e.tail, commentLine("", c.Text))
		return
	}
	switch c.Place {
	case ast.CommentTrailing:
		if l, ok := d.line(anchor); ok {
			e.addTrail(l, c.Text)
			return
		}
		e.atEndOf(d, anchor, c, blank)
	case ast.CommentAtEnd:
		e.atEndOf(d, anchor, c, blank)
	default: // ast.CommentBefore
		if l, ok := d.line(anchor); ok {
			e.addBefore(l.Line, indentOf(l.Col), c.Text, blank)
			return
		}
		e.atEndOf(d, anchor, c, blank)
	}
}

// line is the code line one anchor names inside a declaration: its header
// for "", an edge for "->N", else the line that path opens on.
func (d *textDecl) line(anchor string) (parser.CodeLine, bool) {
	if anchor == "" {
		if d.header.Line == 0 {
			return parser.CodeLine{}, false
		}
		return d.header, true
	}
	if n, ok := edgeOrdinal(anchor); ok {
		l, found := d.edges[n]
		return l, found
	}
	l, found := d.byPath[anchor]
	return l, found
}

// atEndOf writes a comment below the last line of the block an anchor
// names — where the author wrote it, and where a comment whose anchor the
// document lost still says what it said, one block away at worst.
func (e *editor) atEndOf(d *textDecl, anchor string, c *ast.Comment, blank bool) {
	if anchor != "" {
		if end, ok := d.blockEnd[anchor]; ok && end > 0 {
			e.addAfter(end, indentOf(d.blockCol[anchor]), c.Text, blank)
			return
		}
		if l, ok := d.line(anchor); ok {
			// A property with no block under it has no "end" to sit
			// below: written there, the next read gives the comment to
			// the line that follows. Above it, the address holds.
			e.addBefore(l.Line, indentOf(l.Col), c.Text, blank)
			return
		}
	}
	if d.last > 0 {
		e.addAfter(d.last, indentOf(d.bodyCol), c.Text, blank)
		return
	}
	if d.header.Line > 0 {
		// No body the index can address — a `prompt`, whose body is
		// TEXT, or a declaration written bare. Below the header there is
		// nowhere to write a comment: inside a prompt body a `#` is
		// text, so the comment would become part of the prompt. Above
		// the header it stays this declaration's and reads back the same.
		e.addBefore(d.header.Line, indentOf(d.header.Col), c.Text, blank)
		return
	}
	e.tail = append(e.tail, commentLine("", c.Text))
}

func (e *editor) addBefore(line int, indent, text string, blank bool) {
	if e.before == nil {
		e.before = map[int][]string{}
	}
	if blank {
		e.before[line] = append(e.before[line], "")
	}
	e.before[line] = append(e.before[line], commentLine(indent, text))
}

func (e *editor) addAfter(line int, indent, text string, blank bool) {
	if e.after == nil {
		e.after = map[int][]string{}
	}
	if blank {
		e.after[line] = append(e.after[line], "")
	}
	e.after[line] = append(e.after[line], commentLine(indent, text))
}

// addTrail writes a comment at the end of a line. A line can carry ONE —
// two comments written at the end of two source lines the writer folded
// into one (two `- item` lines becoming `tools: [a, b]`) would read back as
// a single comment if they were joined, so the second goes on its own line
// below instead: the text is kept, on the same declaration, and the round
// trip is stable.
func (e *editor) addTrail(l parser.CodeLine, text string) {
	if e.trail == nil {
		e.trail = map[int]string{}
	}
	// The statement's last TOKEN line, never its extent: a `prompt`
	// header reaches to the end of its body, and a comment written there
	// would be body text.
	at := l.TokenEnd
	if at < l.Line {
		at = l.End
	}
	if _, taken := e.trail[at]; taken {
		e.addAfter(at, indentOf(l.Col), text, false)
		return
	}
	e.trail[at] = " " + commentLine("", text)
}

// closeHead puts a blank line between the comments written above line and
// the line itself — what writeHead writes after the file's own head.
func (e *editor) closeHead(line int) {
	if len(e.before[line]) == 0 {
		return
	}
	e.before[line] = append(e.before[line], "")
}

func (e *editor) appendTail(c *ast.Comment) {
	if c.Blank && len(e.tail) > 0 {
		e.tail = append(e.tail, "")
	}
	e.tail = append(e.tail, commentLine("", c.Text))
}

// render writes the lines back with every insertion in place.
func (e *editor) render() string {
	if len(e.before) == 0 && len(e.after) == 0 && len(e.trail) == 0 && len(e.tail) == 0 {
		return strings.Join(e.lines, "\n")
	}
	out := make([]string, 0, len(e.lines)+len(e.before)+len(e.after)+len(e.tail))
	for i, l := range e.lines {
		n := i + 1
		out = append(out, e.before[n]...)
		if t, ok := e.trail[n]; ok {
			l += t
		}
		out = append(out, l)
		out = append(out, e.after[n]...)
	}
	if len(e.tail) > 0 {
		out = appendTailComments(out, e.tail)
	}
	return strings.Join(out, "\n")
}

// appendTailComments puts the file's tail comments after its last line of
// text, keeping the single trailing newline the writer ends on.
func appendTailComments(lines, tail []string) []string {
	last := len(lines)
	for last > 0 && strings.TrimSpace(lines[last-1]) == "" {
		last--
	}
	out := make([]string, 0, len(lines)+len(tail)+1)
	out = append(out, lines[:last]...)
	out = append(out, "")
	out = append(out, tail...)
	out = append(out, lines[last:]...)
	return out
}

// commentLine is one written comment: `## text`, with no trailing space
// when the text is empty — a `##` alone is how a blank line inside a
// comment block is written, and a trailing space there is whitespace every
// editor and every linter strips, which would take the file back out of
// its canonical form.
func commentLine(indent, text string) string {
	if text == "" {
		return indent + "##"
	}
	return indent + "## " + text
}

func indentOf(col int) string {
	if col <= 1 {
		return ""
	}
	return strings.Repeat(" ", col-1)
}

// edgeOrdinal reads an edge anchor (`->3`) back into its ordinal.
func edgeOrdinal(anchor string) (int, bool) {
	rest, ok := strings.CutPrefix(anchor, "->")
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}
