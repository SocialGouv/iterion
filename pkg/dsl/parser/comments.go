package parser

import (
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// Comment provenance — where a `##` line is written, said as an address the
// text and the AST both understand.
//
// A comment is not part of the program, so nothing downstream can tell the
// writer where to put it back. The address below is what stands in: the
// top-level declaration the comment sits in, the dotted key path of the line
// it names inside that declaration's body, and how it sits there (above the
// line, below the block's last line, at the end of the line). Both ends of
// the round trip read it from the SAME walk — Scan over the source tells the
// parser what to attach, Scan over the writer's text tells the writer where
// to put it back (pkg/dsl/unparse) — so the two cannot drift apart: a rule
// read one way in is read the same way out.

// edgeKeyPrefix opens the path of an edge line, which the grammar gives no
// key of its own: `->3` is the third edge line of its declaration.
const edgeKeyPrefix = "->"

// EdgeKey is the path of the nth edge line (1-based) of a declaration.
func EdgeKey(n int) string { return edgeKeyPrefix + strconv.Itoa(n) }

// DeclRef names the top-level declaration a line belongs to: the keyword
// that opens it and the name that follows — "" for the singletons (`vars:`,
// `presets:`, `attachments:`, `secrets:`) and for the `dsl:` header. Kind is
// "" for a line that precedes every declaration: the file itself.
type DeclRef struct {
	Kind string
	Name string
	// Line is the 1-based line of the declaration's header.
	Line int
}

// IsFile reports an address on the file itself rather than on one of its
// declarations.
func (d DeclRef) IsFile() bool { return d.Kind == "" }

// CodeLine is one line of a `.bot` source that carries code, with its
// address: the declaration it belongs to and the dotted key path of the
// property or block it opens.
type CodeLine struct {
	Line int // 1-based line of the line's first token
	// End is the 1-based line the statement ends on: a value written over
	// several lines (a raw string, a block scalar, a prompt body) starts
	// on its own line and ends below it.
	End int
	// Col is the 1-based column of the line's first token — its
	// indentation, in the units the source itself uses.
	Col int
	// TokenEnd is the last line the statement's own TOKENS reach. It is
	// End for every line but a `prompt` header, whose End covers the body
	// below it — and a comment cannot be written at the end of a prompt
	// body, where a `#` is text.
	TokenEnd int
	Decl     DeclRef
	// Path is the dotted key path of the line inside its declaration's
	// body: "model", "sandbox.network.allow", "->2" for the second edge.
	// "" is the declaration's own header line. Keyed is false for a line
	// the grammar gives no key at all (a `- item`, a closing brace); such
	// a line cannot be addressed, and Path then names the block it is in.
	Path  string
	Keyed bool
	// Edge is the 1-based ordinal of the edge line (`a -> b …`) among the
	// edge lines of its declaration, 0 when the line is not one. An edge
	// carries its own comments (ast.Edge.Comments) and is found back by
	// this ordinal: the writer emits one line per edge, in the AST's
	// order, so the nth edge line of the text is the nth edge of the AST.
	Edge int
}

// PlacedComment is one `##` comment with the address it is written at.
type PlacedComment struct {
	Text string
	Line int
	Col  int
	Decl DeclRef
	// Path and Place are ast.Comment's Anchor and Place: the line the
	// comment names inside Decl, and how it sits relative to it.
	Path  string
	Place ast.CommentPlace
	// Blank is ast.Comment's: a blank line separates this comment from
	// the one above it, in the same run.
	Blank bool
	// Edge is the ordinal of the edge line the comment names, 0 when it
	// names none (see CodeLine.Edge).
	Edge int
}

// Scan reads a `.bot` token stream and returns the address of every code
// line and of every `##` comment in it — the single reading of "where is
// this written" that the parser and the writer share.
func Scan(tokens []Token) (lines []CodeLine, comments []PlacedComment) {
	w := &scanner{}
	w.run(tokens)
	return w.lines, w.comments
}

// ScanSource is Scan on text no parser run has tokenised — the writer's
// output.
func ScanSource(name, src string) (lines []CodeLine, comments []PlacedComment) {
	return Scan(NewLexer(name, src).All())
}

// scanner walks the token stream line by line, keeping the enclosing block
// keys on a column-ordered stack.
type scanner struct {
	lines    []CodeLine
	comments []PlacedComment
	// pending are the comments met since the last code line, waiting for
	// the line they lead.
	pending []PlacedComment
	decl    DeclRef
	edges   int
	// stack holds one entry per enclosing block, deepest last: the column
	// its header was written at, and the key it opened.
	stack []blockRef
	// last is the code line before the comments now pending; lastEnd is
	// the line its statement ends on, which is where a trailing comment
	// still belongs to it.
	last     CodeLine
	seenCode bool
	// promptEnd is the last line of the prompt body read since the line
	// being assembled began: a `prompt` header ends where its body does.
	promptEnd int
	// seenDecl closes the file's head: the `dsl:` header and the `import`
	// lines are code but carry no comment, so a comment above them is
	// still the head's — and a migration that inserts a header must not
	// move a single comment.
	seenDecl bool
	lastEnd  int
}

type blockRef struct {
	col int
	key string
	// body is set once a line DEEPER than this one has been read: only
	// then is it a block, and only a block has an end a comment can sit
	// at. A property with no body under it is not one, and treating it as
	// one put the comment back between two siblings, where the next read
	// calls it the second's — the round trip then never settles.
	body bool
}

func (w *scanner) run(tokens []Token) {
	var line []Token
	flush := func() {
		if len(line) > 0 {
			w.codeLine(line)
			line = nil
		}
	}
	for _, t := range tokens {
		switch t.Type {
		case TokenEOF:
			continue
		case TokenIndent, TokenDedent, TokenNewline:
			// Virtual structure carries no address: the columns of the
			// real tokens say everything.
			continue
		case TokenPromptLine:
			// A prompt body is text, never a line of the language; it
			// extends the header above it, which is still being read.
			w.promptEnd = maxInt(w.promptEnd, t.Line)
			w.lastEnd = maxInt(w.lastEnd, t.Line)
			continue
		case TokenComment:
			flush()
			w.comment(t)
			continue
		}
		if len(line) > 0 && t.Line != line[0].Line {
			flush()
		}
		line = append(line, t)
	}
	flush()
	w.flushTail()
}

// lastLineOf is the line a token ends on — the lexer's own measure of its
// extent in the source. A token built by hand (no EndLine) ends where it
// starts.
func lastLineOf(t Token) int {
	if t.EndLine < t.Line {
		return t.Line
	}
	return t.EndLine
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// comment records a `##` line: written on the last statement's own lines it
// is that statement's trailing comment, else it waits for the line it leads.
func (w *scanner) comment(t Token) {
	c := PlacedComment{Text: t.Value, Line: t.Line, Col: t.Column}
	if w.seenCode && t.Line <= w.lastEnd {
		c.Decl, c.Path, c.Edge = w.last.Decl, w.last.Path, w.last.Edge
		c.Place = ast.CommentTrailing
		w.comments = append(w.comments, c)
		return
	}
	if n := len(w.pending); n > 0 && t.Line > w.pending[n-1].Line+1 {
		// A blank line inside the run: the author's paragraph break.
		c.Blank = true
	}
	w.pending = append(w.pending, c)
}

// codeLine records one line of code and hands it the comments written above
// it.
func (w *scanner) codeLine(toks []Token) {
	first := toks[0]
	col := first.Column
	// The blocks open ABOVE this line are what a comment written between
	// the two sits in: taken before the line closes any of them, because
	// closing them is exactly what makes the comment a CommentAtEnd.
	openHere := append([]blockRef(nil), w.stack...)
	// … and whether a declaration had been seen ABOVE the comments now
	// pending: this line may be the first one, and the head is what comes
	// before it.
	underHead := !w.seenDecl
	line := CodeLine{Line: first.Line, End: first.Line, Col: col}
	for _, t := range toks {
		line.End = maxInt(line.End, lastLineOf(t))
	}
	line.TokenEnd = line.End
	line.End = maxInt(line.End, w.promptEnd)
	w.promptEnd = 0

	switch {
	case col == 1 && isTopLevelKeyword(first.Type):
		if first.Type != TokenDSL && first.Type != TokenImport {
			w.seenDecl = true
		}
		w.decl = DeclRef{Kind: first.Value, Name: declName(toks), Line: first.Line}
		w.edges = 0
		w.stack = w.stack[:0]
		line.Decl, line.Keyed = w.decl, true
	default:
		for len(w.stack) > 0 && w.stack[len(w.stack)-1].col >= col {
			w.stack = w.stack[:len(w.stack)-1]
		}
		if n := len(w.stack); n > 0 {
			w.stack[n-1].body = true
			// …on the snapshot too: THIS line is what makes the block
			// above it a block, and the comments pending since then sit
			// inside it.
			if n-1 < len(openHere) {
				openHere[n-1].body = true
			}
		}
		line.Decl = w.decl
		if kind, name, ok := nestedDeclKey(toks); ok {
			// A declaration nested in a `group` body: the same header
			// shape, indented. It is addressed under the group, which
			// is the declaration the file holds.
			key := kind + " " + name
			line.Path, line.Keyed = joinPath(w.stack, key), true
			w.stack = append(w.stack, blockRef{col: col, key: key})
			break
		}
		if n := arrowCount(toks); n > 0 {
			// A chain — `a -> b -> c` — is ONE line and as many edges as
			// it has arrows; the parser appends them in that order. Count
			// the arrows, or every comment below a chain would name an
			// edge too early.
			line.Edge = w.edges + 1
			w.edges += n
			line.Path, line.Keyed = joinPath(w.stack, EdgeKey(line.Edge)), true
			w.stack = append(w.stack, blockRef{col: col, key: EdgeKey(line.Edge)})
			break
		}
		if key, ok := lineKey(toks); ok {
			line.Path, line.Keyed = joinPath(w.stack, key), true
			w.stack = append(w.stack, blockRef{col: col, key: key})
			break
		}
		line.Path = joinPath(w.stack, "")
	}

	// Above the file's FIRST declaration, its own comments and the ones
	// leading that declaration are written one after the other, with only
	// a blank line between them (the writer puts one there —
	// unparse.writeHead). So a run GLUED to the line below leads it, and
	// what a blank line separates from that run is the file's head.
	head := 0
	if underHead {
		head = len(w.pending)
		for i, want := len(w.pending)-1, line.Line-1; i >= 0 && w.pending[i].Line == want; i, want = i-1, want-1 {
			head = i
		}
		// The frontmatter block is never cut: a bot's catalogue identity
		// is read between an opening `## ---` and its closing one
		// (bundle.ParseFrontmatter, on the file's first non-blank lines),
		// and a blank line inside it would otherwise send the second half
		// travelling with the first declaration — leaving code between
		// the two fences and the bot without a name.
		if fm := frontmatterEnd(w.pending); fm > head {
			head = fm
		}
		if head == 0 && len(w.comments) == 0 && !w.seenCode {
			// One run, opening the file, glued to what follows: the head
			// it has always been — the frontmatter and the strict-escape
			// directive are read there — and the round trip keeps it
			// where it is either way.
			head = len(w.pending)
		}
	}
	for i, c := range w.pending {
		w.attachLeading(c, line, openHere, i < head)
	}
	w.pending = w.pending[:0]
	w.lines = append(w.lines, line)
	w.last, w.seenCode, w.lastEnd = line, true, line.End
}

// attachLeading gives one pending comment its address, relative to the code
// line that follows it.
func (w *scanner) attachLeading(c PlacedComment, next CodeLine, open []blockRef, head bool) {
	switch {
	case head:
		// The file's own head, which is where it is already written back
		// (File.Comments).
		c.Place = ast.CommentBefore
		w.comments = append(w.comments, c)
	case c.Col <= next.Col && next.Keyed:
		c.Decl, c.Path, c.Edge, c.Place = next.Decl, next.Path, next.Edge, ast.CommentBefore
		w.comments = append(w.comments, c)
	case !next.Keyed && !w.seenCode:
		// Before any declaration, above a line that takes no comment: the
		// head all the same.
		c.Place = ast.CommentBefore
		w.comments = append(w.comments, c)
	default:
		// Either the block the comment sits in closed under it, or the
		// line below it is one the grammar gives no key (a `- item`, a
		// closing brace): it is written back below the last line of the
		// block it was in — the deepest one still open at its column.
		w.atEnd(&c, open, false)
		w.comments = append(w.comments, c)
	}
}

// atEnd addresses a comment as "below the last line of the block it sits
// in", the block being the deepest one still open at the comment's column.
func (w *scanner) atEnd(c *PlacedComment, open []blockRef, tail bool) {
	c.Place = ast.CommentAtEnd
	if tail && c.Col <= 1 {
		// Past the last line of code, at the left margin: the file's own
		// tail. A comment at the margin with code still below it is not
		// one — it is written inside the declaration it sits in, and
		// giving it to the file would move it to the end of the file.
		c.Decl, c.Path = DeclRef{}, ""
		return
	}
	c.Decl = w.last.Decl
	var keys []string
	for _, b := range open {
		if b.col >= c.Col {
			break
		}
		if !b.body {
			// Not a block: nothing below it belongs to it, so neither
			// does the comment. What holds it is the block above.
			break
		}
		keys = append(keys, b.key)
	}
	c.Path = strings.Join(keys, ".")
}

// flushTail addresses the comments no line of code follows.
func (w *scanner) flushTail() {
	for _, c := range w.pending {
		if !w.seenCode {
			c.Place = ast.CommentBefore
			w.comments = append(w.comments, c)
			continue
		}
		w.atEnd(&c, w.stack, true)
		w.comments = append(w.comments, c)
	}
	w.pending = nil
}

// declName is the name a declaration header gives itself — the identifier
// after the keyword — "" for the singletons. A `use X as Y` is named by its
// prefix, which is what names its nodes.
func declName(toks []Token) string {
	if len(toks) < 2 {
		return ""
	}
	if toks[0].Type == TokenUse {
		for i := 1; i+1 < len(toks); i++ {
			if toks[i].Type == TokenAs {
				return toks[i+1].Value
			}
		}
		return toks[1].Value
	}
	if toks[1].Type == TokenIdent || isKeywordToken(toks[1].Type) || toks[1].Type == TokenString {
		return toks[1].Value
	}
	return ""
}

// lineKey is the property or entry a line opens — the identifier before its
// colon — and whether the line has one at all.
func lineKey(toks []Token) (string, bool) {
	if len(toks) < 2 || toks[1].Type != TokenColon {
		return "", false
	}
	if toks[0].Type != TokenIdent && !isKeywordToken(toks[0].Type) {
		return "", false
	}
	return toks[0].Value, true
}

// arrowCount is the number of edges an edge line declares — one per `->`.
// The arrow is a token, so a `->` inside a string is text, as it is to the
// parser.
func arrowCount(toks []Token) int {
	n := 0
	for _, t := range toks {
		if t.Type == TokenArrow {
			n++
		}
	}
	return n
}

func joinPath(stack []blockRef, key string) string {
	if len(stack) == 0 {
		return key
	}
	keys := make([]string, 0, len(stack)+1)
	for _, b := range stack {
		keys = append(keys, b.key)
	}
	if key != "" {
		keys = append(keys, key)
	}
	return strings.Join(keys, ".")
}

// nestedDeclKey reads the header of a declaration written inside another
// one — a `group` member — which the grammar writes `<keyword> <name>:`
// exactly as at the left margin. A keyword followed straight by a colon is
// a block property (`vars:` under a workflow), not one of these.
func nestedDeclKey(toks []Token) (kind, name string, ok bool) {
	if len(toks) < 3 || !isTopLevelKeyword(toks[0].Type) || toks[1].Type == TokenColon {
		return "", "", false
	}
	n := declName(toks)
	if n == "" {
		return "", "", false
	}
	for _, t := range toks {
		if t.Type == TokenColon {
			return toks[0].Value, n, true
		}
	}
	return "", "", false
}

// frontmatterEnd is the number of pending comments the frontmatter block
// takes — its opening fence through its closing one — or 0 when the run
// does not open with a fence.
func frontmatterEnd(pending []PlacedComment) int {
	if len(pending) == 0 || strings.TrimSpace(pending[0].Text) != workflowfile.FrontmatterFence {
		return 0
	}
	for i := 1; i < len(pending); i++ {
		if strings.TrimSpace(pending[i].Text) == workflowfile.FrontmatterFence {
			return i + 1
		}
	}
	return 0
}
