package parser

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/SocialGouv/iterion/pkg/dsl/readbudget"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// Safety limits to prevent DoS from malicious .bot files.
const (
	maxSourceSize   = 10 * 1024 * 1024 // 10 MB max file size
	maxNestingDepth = 100              // max indentation nesting levels
)

// ReadBudgetEnv names the operator's override of the read budget
// (readbudget.Env).
const ReadBudgetEnv = readbudget.Env

// DefaultReadBudget is how much one bot may ask the reader to record
// (readbudget.Default): 2^20 records of its .bot files together — every
// word, value, punctuation mark, prompt line (blank or not), line end,
// indentation step and the end of each file is one, a comment two
// (commentRecords) — and 2^20 of a YAML twin's reading, a value one and a
// comment line two (pkg/dsl/author). The size cap bounds each file's bytes;
// the budget bounds what they turn into, which a dense shape makes hundreds
// of bytes of memory per byte of source (one flat list of one-letter names,
// a line of commas, a run of comment lines). The lexer spends it as it reads,
// so a bot past it is refused before the work it asks for, not after; the
// files of one bot share it (NewUnitBudget), so splitting a bot into
// fragments does not multiply what one load of it costs. An expression lives
// inside one string, which the lexer counts as a single record: the
// expression parser spends the same budget on its tokens (pkg/dsl/expr).
const DefaultReadBudget = readbudget.Default

// commentRecords is what a comment spends of the read budget. A token the
// parser reads and drops costs the readers its token; a comment is
// addressed by the comment scan, kept in the AST and written back by the
// canonical form, about twice that — measured at the budget's edge, a run
// of comment lines against a flat list of names: 507 against 284 MiB of
// peak memory on /api/parse's work, 826 against 407 on the canonical form.
const commentRecords = 2

// ReadBudget is the budget in force (readbudget.Read): DefaultReadBudget, or
// the count the operator sets in ITERION_DSL_READ_BUDGET where the source is
// read — 0 removes the budget. Every reader of a bot resolves it there, the
// YAML twin and the expression parser included, so one knob governs them all
// and a level added to the resolver reaches each at once.
func ReadBudget() int {
	return readbudget.Read()
}

// ReadBudgetMessage is the refusal of a source past budget, the wording
// every reader of the budget gives (the lexer, the YAML twin's speller).
func ReadBudgetMessage(budget int) string {
	return fmt.Sprintf("the source is past the read budget of %d — every word, value, punctuation mark, prompt line, line end, indentation step and the file's end spends one, a comment two: the reader stops here", budget)
}

// UnitBudgetMessage is the refusal of a bot whose files together are past
// the budget they share, said in the file — or at the import of the file —
// the budget ran out in.
func UnitBudgetMessage(budget int) string {
	return fmt.Sprintf("the bot is past the read budget of %d, which its files share — every word, value, punctuation mark, prompt line, line end, indentation step and file's end spends one, a comment two: the reader stops here", budget)
}

// ReadBudgetHint is the remedy of a refusal for the read budget: the
// operator's count. Splitting a bot into fragments is none, since its files
// share the budget.
const ReadBudgetHint = "The budget is " + ReadBudgetEnv + " wherever a bot is read — the CLI, the server, the runner — and the files of a bot share it: a larger count raises it, 0 removes it."

// The remedies of the two refusals of a whole source. A source past a bound
// is no misplaced line, so E001's catalogued hint — about a line's
// indentation — would send its author to the wrong place. The size cap is a
// file's, so a large bot is split into fragments under it; the read budget
// is the bot's.
const (
	sizeRefusalHint = "A bot this large reads in parts: move declarations into fragments under lib/ (`import \"lib/<name>.bot\"`), each read on its own 10 MiB — the read budget stays the bot's."
	readBudgetHint  = ReadBudgetHint
)

// A Budget is a read budget and what the sources reading on it spent: one
// source's own (Parse, NewLexer), or the one the files of a bot share
// (NewUnitBudget) — a bot written in ten files costs its reader what the
// same bot in one file does, never ten times it.
type Budget struct {
	limit, spent int
	// unit marks a bot's budget: its refusal names the bot, whichever of
	// its files ran out.
	unit bool
}

// NewUnitBudget is the budget in force (ReadBudget) for the files of one
// bot to share, of which spent is already taken — what the reading of a
// main its caller parsed spent (an author document's).
func NewUnitBudget(spent int) *Budget {
	return &Budget{limit: ReadBudget(), spent: spent, unit: true}
}

// sourceBudget is one source's own budget.
func sourceBudget() *Budget {
	return &Budget{limit: ReadBudget()}
}

// Spent is what the sources reading on b took of it.
func (b *Budget) Spent() int { return b.spent }

// Left is what is left of b.
func (b *Budget) Left() int { return b.limit - b.spent }

// Limit is b's count of records.
func (b *Budget) Limit() int { return b.limit }

// Bound names the bound a source was refused whole for (Lexer.Refused,
// ParseResult.Refused).
type Bound int

const (
	// BoundNone: the source reads.
	BoundNone Bound = iota
	// BoundSize: the source is past the 10 MiB size cap.
	BoundSize
	// BoundBudget: the source is past the read budget.
	BoundBudget
)

// Lexer tokenizes an iterion DSL source file with indent-sensitive INDENT/DEDENT tokens.
type Lexer struct {
	src  []rune
	file string // source filename

	pos  int // current index in src
	line int // 1-based
	col  int // 1-based

	// tokens is the stream's materialized prefix: every token up to the
	// frontier, which the cursor never passes (ti <= len(tokens)). What the
	// stream still holds past the frontier lives in ahead, the splice queue:
	// a resync that splices a broken list's closing DEDENTs cuts the tail
	// loose into it instead of recopying it (splice). Before the first
	// splice the queue is empty and tokens is the whole stream, so a reading
	// that never rescues pays nothing for it.
	tokens []Token
	ti     int // current read index into the stream; tokens[:ti] is consumed
	ahead  tokenQueue

	indentStack []int // stack of indentation levels (in spaces); starts with [0]
	atLineStart bool  // true when we are at the beginning of a (possibly indented) line

	// promptMode: when true, lines are emitted as TokenPromptLine until dedent
	promptMode      bool
	promptBodyLevel int // the indent level of prompt body lines (level of prompt decl + indent unit)

	// strictEscape: when true, "..." strings interpret standard escape
	// sequences (\", \\, \n, \t, \r). Opt-in via a `## strict-escape: on`
	// directive at the top of the file. Default false (legacy behaviour:
	// every \X is preserved verbatim for downstream layers to handle).
	strictEscape bool

	// profile is the syntax profile the file declares in its `dsl: N`
	// header (parser.Preamble), 1 when it declares none. Read before
	// tokenising, as the escape mode is: profile 2 reads standard escapes
	// with no directive.
	profile int

	// lineStarts is the rune index of each line's first rune (line 1 at
	// 0): what turns a token's (Line, Column) into its Offset.
	lineStarts []int

	// profileReads are the places a profile-1 file would be read otherwise
	// under profile 2 (ProfileRead); pendingBlanks holds the blank lines of
	// the prompt body being read until a further body line proves them
	// interior — the trailing ones are dropped by both profiles.
	profileReads  []ProfileRead
	pendingBlanks []ProfileRead

	// blockScalarMode: when true, lines are accumulated into blockScalarBuf
	// until we see a line less indented than blockScalarBaseLevel. Triggered
	// by `|` immediately following a colon, YAML-style.
	blockScalarMode      bool
	blockScalarBuf       []rune
	blockScalarBaseLevel int // -1 means "use first non-blank content line's indent"
	blockScalarStartLine int
	blockScalarStartCol  int

	// budget is the read budget this source spends (spend): its own, or
	// the one the files of its bot share. What it spent is the records it
	// made — tokens, and the blank prompt lines profile 1 notes instead of
	// a token.
	budget *Budget
	// refused is the bound the whole source was refused for, and
	// refusedHint that refusal's remedy (refuseWhole); BoundNone and ""
	// while the source reads.
	refused     Bound
	refusedHint string
}

// Refused is the bound the lexer refused the whole source for — its token
// stream then the one refusal — BoundNone when the source reads.
func (l *Lexer) Refused() Bound {
	return l.refused
}

// refusal is the refusal of the whole source as the parser reports it
// (addErrorHint, named file), nil while the source reads.
func (l *Lexer) refusal(file string) *ReadRefusal {
	if l.refused == BoundNone {
		return nil
	}
	t := l.tokens[0]
	return &ReadRefusal{Bound: l.refused, Diagnostic: Diagnostic{
		Code:     lexerCode(t),
		Severity: SeverityError,
		Message:  t.Value,
		File:     file,
		Line:     t.Line,
		Column:   t.Column,
		Hint:     l.refusedHint,
	}}
}

// NewLexer creates a new Lexer for the given source, on a budget of its
// own.
func NewLexer(filename, src string) *Lexer {
	return lexSource(filename, src, sourceBudget())
}

// lexSource is NewLexer spending budget.
func lexSource(filename, src string, budget *Budget) *Lexer {
	if len(src) > maxSourceSize {
		l := &Lexer{file: filename, line: 1, col: 1, budget: budget}
		l.refuseWhole(BoundSize, fmt.Sprintf("source file exceeds maximum size (%d bytes > %d)", len(src), maxSourceSize), sizeRefusalHint, 1, 1)
		return l
	}
	// Normalize the source before tokenising:
	//  - strip a leading UTF-8 BOM (macOS / Windows editors occasionally
	//    save it; the lexer would otherwise emit a confusing
	//    TokenError on line 1 referencing U+FEFF).
	//  - collapse CRLF to LF so the indent-sensitive line handling and
	//    string-literal scanner don't drag \r into tokens or buffers.
	//    Stray lone \r is left alone — that's vanishingly rare and a
	//    legitimate-as-content scenario in heredocs.
	src = NormalizeSource(src)
	// The head of the file decides how the rest is read (ReadPreamble): a
	// header the parser will refuse (E040) reads as profile 1 meanwhile.
	pre := ReadPreamble(src)
	profile := max(pre.Profile, 1)
	if profile > MaxProfile {
		// The parser refuses it (E040); the strings are read as profile 1
		// meanwhile, never with a profile this build knows nothing of.
		profile = 1
	}
	return newLexer(filename, src, profile, pre.StrictEscape || profile >= 2, budget)
}

// newLexer is NewLexer with the profile and the escape mode decided by the
// caller instead of read off the source's head: a fragment of text — an
// edge line read on its own (ParseEdgeLine) — has no head to read them
// from. src is already normalised; budget is what the source spends.
func newLexer(filename, src string, profile int, strictEscape bool, budget *Budget) *Lexer {
	l := &Lexer{
		src:          []rune(src),
		file:         filename,
		line:         1,
		col:          1,
		indentStack:  []int{0},
		atLineStart:  true,
		strictEscape: strictEscape,
		profile:      profile,
		budget:       budget,
	}
	// One entry per line, sized from the newline count: grown by append, a
	// source of blank lines would allocate five times the table it ends
	// with.
	l.lineStarts = make([]int, 1, strings.Count(src, "\n")+1)
	for i, r := range l.src {
		if r == '\n' {
			l.lineStarts = append(l.lineStarts, i+1)
		}
	}
	l.tokenize()
	return l
}

// NormalizeSource is the text every reader of a file sees: the BOM
// stripped and CRLF folded to LF (the two things NewLexer does before
// tokenising). ReadPreamble expects this form, so a reader that asks the
// head of a file on disk — the studio's save guard — normalises through
// here rather than in its own way.
func NormalizeSource(src string) string {
	src = strings.TrimPrefix(src, "\ufeff")
	return strings.ReplaceAll(src, "\r\n", "\n")
}

// dashOpensItem reports whether a `-` at the current position opens a list
// item — the YAML-style form of a list, one `- item` per line under the
// property: first token on its line, followed by a space, a tab, a newline
// or the end of the file. Anywhere else `-` is not a token of the language
// (`->` is), and a prompt body, a block scalar or a raw string never reach
// this scanner.
//
// "First on its line" is read from the source, not from the token stream:
// a trailing comment stands in for the newline it consumed (scanComment),
// so on the line after `- bash ## note` the previous TOKEN is the element,
// while only indentation precedes the `-` on its own line.
func (l *Lexer) dashOpensItem() bool {
	if l.line-1 >= len(l.lineStarts) {
		return false
	}
	for i := l.lineStarts[l.line-1]; i < l.pos && i < len(l.src); i++ {
		if l.src[i] != ' ' && l.src[i] != '\t' {
			return false
		}
	}
	if l.pos+1 >= len(l.src) {
		return true
	}
	switch l.src[l.pos+1] {
	case ' ', '\t', '\n':
		return true
	}
	return false
}

// ProfileRead is one place where a file read as profile 1 would be read
// otherwise under profile 2: an `escape` — a backslash inside a `"…"`
// literal, kept verbatim by profile 1 and decoded by profile 2 — or a
// `paragraph` — a blank line inside a prompt body, dropped by profile 1 and
// kept by profile 2. What `iterion validate` reports (C144) on a file with
// no `dsl:` header, so the profile it is read in is a choice and not a
// default nobody noticed.
type ProfileRead struct {
	Line int
	Kind string
}

// ProfileReads lists the places profile 2 would read otherwise, in order
// of appearance. Empty for a profile-2 source by construction: its escapes
// take the strict branch and its blank body lines become tokens, so the
// scanners never note one.
func (l *Lexer) ProfileReads() []ProfileRead {
	return l.profileReads
}

// Profile is the syntax profile the source declared (1 when it declared
// none, or a header the parser refuses), the reading every string of the
// token stream got.
func (l *Lexer) Profile() int {
	return max(l.profile, 1)
}

// detectStrictEscape scans the first directives at the top of the file
// (leading `## key: value` comments before any significant token) and
// returns true if `## strict-escape: on` is present. Recipes opt into
// standard `"..."` escape interpretation by adding this directive as
// the first or among the first comment lines.
func detectStrictEscape(src string) bool {
	lines := strings.SplitN(src, "\n", 32)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// A comment is a `#` or `##` line — the same rule the lexer applies
		// (workflowfile.CommentText is the shared definition), so a plain
		// `# note` above the directive neither hides it nor ends the scan.
		body, ok := workflowfile.CommentText(line)
		if !ok {
			return false
		}
		body = strings.TrimSpace(body)
		// Accept `strict-escape: on` (with optional surrounding whitespace
		// already trimmed) and a few cosmetic variants.
		if body == "strict-escape: on" || body == "strict-escape:on" || body == "strict-escape = on" {
			return true
		}
	}
	return false
}

// All returns every token of the stream (for debugging): the materialized
// prefix completed by what the splice queue still holds, which the join
// drains.
func (l *Lexer) All() []Token {
	for {
		t, ok := l.ahead.pop()
		if !ok {
			return l.tokens
		}
		l.tokens = append(l.tokens, t)
	}
}

// streamLen is the length of the whole stream: the materialized prefix plus
// what the splice queue still holds.
func (l *Lexer) streamLen() int { return len(l.tokens) + l.ahead.queued }

// Next returns the next token.
func (l *Lexer) Next() Token {
	if l.ti < len(l.tokens) {
		t := l.tokens[l.ti]
		l.ti++
		return t
	}
	if t, ok := l.ahead.pop(); ok {
		// A token leaves the queue for the materialized prefix: the consumed
		// stream (openBlocks) and the lookbacks behind the cursor read it
		// there.
		l.tokens = append(l.tokens, t)
		l.ti++
		return t
	}
	return Token{Type: TokenEOF, Line: l.line, Column: l.col}
}

// Peek returns the next token without consuming it.
func (l *Lexer) Peek() Token {
	if l.ti < len(l.tokens) {
		return l.tokens[l.ti]
	}
	if t, ok := l.ahead.peekAt(0); ok {
		return t
	}
	return Token{Type: TokenEOF, Line: l.line, Column: l.col}
}

// Backup unreads the last consumed token.
func (l *Lexer) Backup() {
	if l.ti > 0 {
		l.ti--
	}
}

// PeekAt returns the token n positions ahead without consuming anything
// (PeekAt(0) is Peek).
func (l *Lexer) PeekAt(n int) Token {
	if i := l.ti + n; i < len(l.tokens) {
		return l.tokens[i]
	} else if t, ok := l.ahead.peekAt(i - len(l.tokens)); ok {
		return t
	}
	return Token{Type: TokenEOF, Line: l.line, Column: l.col}
}

// splice inserts toks ahead of the stream position at — an absolute index in
// the stream: the materialized prefix, then the queue — at or ahead of the
// cursor, where the parser's resyncs always stand. Behind the cursor (a
// test's doing, never a rescue's) the insert is the plain recopy.
func (l *Lexer) splice(at int, toks []Token) {
	switch {
	case at < l.ti:
		l.tokens = slices.Insert(l.tokens, at, toks...)
	case at < len(l.tokens):
		// The splice point is materialized but unread: the tail moves to the
		// queue — a slice header, not a copy — with the inserted tokens ahead
		// of it. The prefix is clipped so a later append can never write
		// into the tail's backing array.
		tail := l.tokens[at:]
		l.tokens = l.tokens[:at:at]
		l.ahead.prepend(tail)
		l.ahead.insertAt(0, toks)
	default:
		l.ahead.insertAt(at-len(l.tokens), toks)
	}
}

// qseg is one segment of the unread stream the splice queue holds: a run of
// the lexed stream a splice cut loose, or one rescue's spliced DEDENTs. A
// segment's toks never change once linked — a split only reslices — so two
// segments may share a backing array and neither can clobber the other.
type qseg struct {
	toks       []Token
	prev, next *qseg
}

// tokenQueue is the unread tail of the token stream past the materialized
// prefix: segments in stream order, plus a scan cache for the resync's
// peeks. It exists so a rescue's splice is a cut-and-join of links — O(1) at
// the cursor, and O(1) at the point the close-point scan just walked to —
// where inserting into the flat slice recopied the whole tail of the stream
// at every rescue: quadratic on a source whose broken lists repeat (#2880).
type tokenQueue struct {
	head   *qseg // first segment holding unread tokens
	off    int   // how far into head.toks the reading stands
	queued int   // the unread tokens the queue holds, over all segments

	// cacheK, cacheNode and cacheOff are where the last peekAt answer
	// stands: cacheK its offset from the frontier (the materialized
	// prefix's length), cacheNode and cacheOff the segment and index
	// holding it. A nil cacheNode is no cache. A pop moves the frontier one
	// closer to it, an insert at or before it pushes it one run further;
	// both adjust it.
	cacheK    int
	cacheNode *qseg
	cacheOff  int
}

// pop consumes the queue's head token.
func (q *tokenQueue) pop() (Token, bool) {
	if q.head == nil {
		return Token{}, false
	}
	t := q.head.toks[q.off]
	q.off++
	q.queued--
	if q.cacheNode != nil {
		q.cacheK--
		if q.cacheK < 0 {
			q.cacheNode = nil
		}
	}
	if q.off == len(q.head.toks) {
		q.head = q.head.next
		if q.head != nil {
			q.head.prev = nil
		}
		q.off = 0
	}
	return t, true
}

// peekAt answers the token k positions into the queue, caching where the
// walk ended: the resync's scans peek at ever-growing offsets, so the next
// peek resumes where this one stopped and a scan of the whole queue costs
// one step per token in it, never one walk per peek.
func (q *tokenQueue) peekAt(k int) (Token, bool) {
	if k < 0 || k >= q.queued {
		return Token{}, false
	}
	node, off, at := q.head, q.off, 0
	if q.cacheNode != nil && q.cacheK <= k {
		node, off, at = q.cacheNode, q.cacheOff, q.cacheK
	}
	for at < k {
		off++
		if off == len(node.toks) {
			node = node.next
			off = 0
		}
		at++
	}
	q.cacheK, q.cacheNode, q.cacheOff = k, node, off
	return node.toks[off], true
}

// prepend links toks in front of every segment the queue holds: the tail a
// splice cut loose. The frontier moved back by len(toks), so every position
// the cache holds shifts by it.
func (q *tokenQueue) prepend(toks []Token) {
	if len(toks) == 0 {
		return
	}
	q.dropConsumed()
	q.queued += len(toks)
	if q.cacheNode != nil {
		q.cacheK += len(toks)
	}
	seg := &qseg{toks: toks, next: q.head}
	if q.head != nil {
		q.head.prev = seg
	}
	q.head, q.off = seg, 0
}

// dropConsumed drops the head segment's consumed prefix, so position 0 of
// the queue is the head node's own start and no link joins ahead of tokens
// already read — a link that kept them would serve them twice and part the
// chain from the count. The scan cache keeps its token.
func (q *tokenQueue) dropConsumed() {
	if q.head == nil || q.off == 0 {
		return
	}
	q.head.toks = q.head.toks[q.off:]
	if q.cacheNode == q.head {
		q.cacheOff -= q.off
	}
	q.off = 0
}

// insertAt links toks ahead of the token k positions into the queue (k == 0
// is the head); an empty toks links nothing. The walk to k resumes at the
// scan cache when it stands on the way: a rescue splices where its
// close-point scan just stopped, so the splice is a link join, never a walk
// back and never a copy.
func (q *tokenQueue) insertAt(k int, toks []Token) {
	if len(toks) == 0 {
		return
	}
	n := len(toks)
	q.queued += n
	if q.head == nil {
		q.head = &qseg{toks: toks}
		return
	}
	q.dropConsumed()
	if k == 0 {
		seg := &qseg{toks: toks, next: q.head}
		q.head.prev, q.head = seg, seg
		if q.cacheNode != nil {
			q.cacheK += n
		}
		return
	}
	node, off, at := q.head, 0, 0
	var prev *qseg
	if q.cacheNode != nil && q.cacheK <= k {
		node, off, at, prev = q.cacheNode, q.cacheOff, q.cacheK, q.cacheNode.prev
	}
	for at < k && node != nil {
		off++
		if off == len(node.toks) {
			prev, node, off = node, node.next, 0
		}
		at++
	}
	if node == nil {
		// k reaches past the queue's end — no rescue's splice does (its
		// index names a token that exists): link at the end.
		ded := &qseg{toks: toks, prev: prev}
		if prev != nil {
			prev.next = ded
		} else {
			q.head = ded
		}
		if q.cacheNode != nil && q.cacheK >= k {
			q.cacheK += n
		}
		return
	}
	var right *qseg
	if off == 0 {
		ded := &qseg{toks: toks, prev: node.prev, next: node}
		if node.prev != nil {
			node.prev.next = ded
		} else {
			q.head = ded
		}
		node.prev = ded
	} else {
		right = &qseg{toks: node.toks[off:], prev: node, next: node.next}
		ded := &qseg{toks: toks, prev: node, next: right}
		right.prev = ded
		if node.next != nil {
			node.next.prev = right
		}
		node.next, node.toks = ded, node.toks[:off]
	}
	// The scan cache holds a position: every token at or past k moved one
	// run further, and one cached in the split-away part of node moved with
	// it.
	if q.cacheNode != nil && q.cacheK >= k {
		if q.cacheNode == node && right != nil {
			q.cacheNode, q.cacheOff = right, q.cacheOff-off
		}
		q.cacheK += n
	}
}

// ---------------- internal ----------------

func (l *Lexer) tokenize() {
	for l.pos < len(l.src) {
		if l.atLineStart {
			l.handleLineStart()
			continue
		}
		l.scanToken()
	}
	// If we ended the file mid-block-scalar, emit the accumulated string
	// plus the virtual newline that terminates the value.
	if l.blockScalarMode {
		l.emit(TokenString, string(l.blockScalarBuf), l.blockScalarStartLine, l.blockScalarStartCol)
		l.emit(TokenNewline, "", l.blockScalarStartLine, l.blockScalarStartCol)
		l.blockScalarMode = false
		l.blockScalarBuf = nil
	}
	// Emit remaining DEDENTs at EOF
	for len(l.indentStack) > 1 {
		l.indentStack = l.indentStack[:len(l.indentStack)-1]
		l.emit(TokenDedent, "", l.line, l.col)
	}
	l.emit(TokenEOF, "", l.line, l.col)
}

// handleLineStart processes leading whitespace, blank lines, comments, and emits INDENT/DEDENT.
func (l *Lexer) handleLineStart() {
	// Count leading spaces
	spaces := 0
	startLine := l.line
	for l.pos < len(l.src) && l.src[l.pos] == ' ' {
		spaces++
		l.advance()
	}

	// Reject tabs as indentation: the DSL is indent-significant via
	// spaces only. A silent tab would be counted as 0-indent, then
	// swallowed as inline whitespace by scanToken, parsing the line at
	// top-level with a cascade of misleading "unexpected token" errors
	// on later lines. Surface the cause directly. Permitted in block-
	// scalar mode (heredocs preserve tab content verbatim), in prompt
	// body lines (handled below the dispatch), and on the line that OPENS
	// a prompt body: tab-indented code pasted as a prompt's first line is
	// content, as it is on every later line, and prompt mode is only armed
	// at the INDENT below — so that line has to be told apart here.
	if !l.blockScalarMode && !l.promptMode && !l.promptBodyOpensHere(spaces) && l.pos < len(l.src) && l.src[l.pos] == '\t' {
		l.emitError(DiagBadIndentation, "tabs are not allowed for indentation; use spaces", startLine, spaces+1)
		// Consume the rest of the line so we don't loop on the same tab.
		for l.pos < len(l.src) && l.src[l.pos] != '\n' {
			l.advance()
		}
		if l.pos < len(l.src) {
			l.advance() // consume '\n'
		}
		return
	}

	// Block scalar mode owns line handling end-to-end (including blank lines,
	// which are preserved as empty content lines, and the terminating
	// less-indented line which dedents out of the block).
	if l.blockScalarMode {
		l.handleBlockScalarLine(spaces, startLine)
		return
	}

	// Blank line or end of file — skip
	if l.pos >= len(l.src) || l.src[l.pos] == '\n' {
		if l.promptMode && l.pos < len(l.src) {
			if l.profile > 1 {
				// Profile 2 keeps a paragraph break inside a prompt body: an
				// empty prompt line, whatever spaces the line held. The
				// parser trims trailing ones, so the blank line after a body
				// is not part of it.
				l.emit(TokenPromptLine, "", l.line, 1)
			} else {
				// Profile 1 skips every blank line — the model gets one
				// newline for a paragraph break — a rule frozen with it.
				// Noted as a place profile 2 reads otherwise, once a further
				// body line proves the blank line interior: a record like
				// profile 2's empty prompt line, so it spends the budget as
				// that token does.
				if !l.spend(l.line, 1, 1) {
					return
				}
				l.pendingBlanks = append(l.pendingBlanks, ProfileRead{Line: l.line, Kind: "paragraph"})
			}
		}
		if l.pos < len(l.src) {
			l.advance() // consume '\n'
		}
		// stay at line start
		return
	}

	// If in prompt mode, emit raw lines until we see less indentation
	if l.promptMode {
		if spaces < l.promptBodyLevel {
			// End prompt mode, fall through to normal indent handling. The
			// blank lines since the last body line were trailing: both
			// profiles drop them.
			l.promptMode = false
			l.pendingBlanks = nil
			// Emit DEDENT for the prompt body block
			if len(l.indentStack) > 1 && l.indentStack[len(l.indentStack)-1] >= l.promptBodyLevel {
				l.indentStack = l.indentStack[:len(l.indentStack)-1]
				l.emit(TokenDedent, "", startLine, 1)
			}
		} else {
			l.emitPromptLine(spaces)
			return
		}
	}

	// Comment lines at line start: `#` or `##` (see scanComment). The one
	// exception is the first line of a prompt body: prompt mode only starts
	// when the INDENT below is emitted, so a `# Heading` opening the body
	// (the ordinary markdown shape of a human node's instructions) must
	// reach that INDENT as text rather than vanish as a comment.
	if l.pos < len(l.src) && l.src[l.pos] == '#' && !l.promptBodyOpensHere(spaces) {
		l.scanComment(startLine)
		l.atLineStart = true
		return
	}

	// Emit INDENT/DEDENT based on indentation change
	currentLevel := l.indentStack[len(l.indentStack)-1]
	if spaces > currentLevel {
		if len(l.indentStack) >= maxNestingDepth {
			l.emitError(DiagBadIndentation, fmt.Sprintf("maximum nesting depth exceeded (%d levels)", maxNestingDepth), startLine, 1)
			return
		}
		l.indentStack = append(l.indentStack, spaces)
		l.emit(TokenIndent, "", startLine, 1)
		// Check if we just entered a prompt body
		if l.isPromptIndent() {
			l.promptMode = true
			l.promptBodyLevel = spaces
			// Emit first prompt line
			l.emitPromptLine(spaces)
			return
		}
	} else if spaces < currentLevel {
		for len(l.indentStack) > 1 && l.indentStack[len(l.indentStack)-1] > spaces {
			l.indentStack = l.indentStack[:len(l.indentStack)-1]
			l.emit(TokenDedent, "", startLine, 1)
		}
		// Verify alignment
		if l.indentStack[len(l.indentStack)-1] != spaces {
			l.emitError(DiagBadIndentation, "indentation does not match any outer level", startLine, 1)
		}
	}

	l.atLineStart = false
}

// promptBodyOpensHere reports whether a line indented by `spaces` is the
// first line of a prompt body: it is deeper than the current level and the
// tokens emitted so far end with the `prompt <name>:` header. It is the
// pre-INDENT twin of isPromptIndent, consulted before a `#` on such a line
// could be scanned as a comment.
func (l *Lexer) promptBodyOpensHere(spaces int) bool {
	if spaces <= l.indentStack[len(l.indentStack)-1] {
		return false
	}
	return l.promptHeaderEndsAt(len(l.tokens) - 1)
}

// isPromptIndent checks if the last emitted tokens before the INDENT are: prompt IDENT : NEWLINE INDENT
func (l *Lexer) isPromptIndent() bool {
	// The INDENT just emitted sits at len-1; the header ends before it.
	return l.promptHeaderEndsAt(len(l.tokens) - 2)
}

// promptHeaderEndsAt reports whether the significant tokens ending at index
// idx are the suffix of a prompt header, `prompt <name>:` followed by an
// optional NEWLINE. Comment tokens are skipped: a trailing `# why` on the
// header line is a comment, not a token of the header, and must not hide the
// body that follows (scanComment consumes the newline, so the Newline token
// may be absent after one).
func (l *Lexer) promptHeaderEndsAt(idx int) bool {
	idx = l.significantIndexAt(idx)
	if idx >= 0 && l.tokens[idx].Type == TokenNewline {
		idx = l.significantIndexAt(idx - 1)
	}
	if idx >= 0 && l.tokens[idx].Type == TokenColon {
		idx = l.significantIndexAt(idx - 1)
	} else {
		return false
	}
	if idx >= 0 && (l.tokens[idx].Type == TokenIdent || isKeywordToken(l.tokens[idx].Type)) {
		idx = l.significantIndexAt(idx - 1)
	} else {
		return false
	}
	return idx >= 0 && l.tokens[idx].Type == TokenPrompt
}

// significantIndexAt returns the index of the last non-comment token at or
// before idx, or -1.
func (l *Lexer) significantIndexAt(idx int) int {
	for idx >= 0 && l.tokens[idx].Type == TokenComment {
		idx--
	}
	return idx
}

// emitPromptLine captures the rest of the current line as a prompt text line.
func (l *Lexer) emitPromptLine(leadingSpaces int) {
	// A further body line: the blank lines before it were interior.
	l.profileReads = append(l.profileReads, l.pendingBlanks...)
	l.pendingBlanks = nil
	startLine := l.line
	// Compute relative indentation: subtract the prompt body base level
	relativeSpaces := leadingSpaces - l.promptBodyLevel
	prefix := ""
	if relativeSpaces > 0 {
		prefix = strings.Repeat(" ", relativeSpaces)
	}

	var buf []rune
	buf = append(buf, []rune(prefix)...)
	for l.pos < len(l.src) && l.src[l.pos] != '\n' {
		buf = append(buf, l.src[l.pos])
		l.advance()
	}
	if l.pos < len(l.src) {
		l.advance() // consume '\n'
	}
	l.emit(TokenPromptLine, string(buf), startLine, 1)
	l.atLineStart = true
}

// scanComment consumes a comment to the end of its line. A comment opens
// with `#`; the traditional `##` form is the same comment with one more
// hash, so both `# note` and `## note` carry the text "note". Outside a
// string, a prompt body or a block scalar a `#` never means anything else
// in the language, so accepting the single form costs no ambiguity — and
// it is the form every YAML-trained author (human or model) reaches for.
func (l *Lexer) scanComment(startLine int) {
	startCol := l.col
	l.advance() // skip the opening #
	if l.pos < len(l.src) && l.src[l.pos] == '#' {
		l.advance() // the `##` form: skip the second #
	}
	var buf []rune
	for l.pos < len(l.src) && l.src[l.pos] != '\n' {
		buf = append(buf, l.src[l.pos])
		l.advance()
	}
	if l.pos < len(l.src) {
		l.advance() // consume '\n'
	}
	// The text is what the shared definition says it is — one space after
	// the hashes removed and nothing more — so the indentation an author
	// wrote inside a comment is part of it and a rewrite puts it back.
	l.emit(TokenComment, workflowfile.CommentBody(string(buf)), startLine, startCol)
	l.atLineStart = true
}

func (l *Lexer) scanToken() {
	// Skip inline whitespace (spaces/tabs that are not at line start)
	for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t') {
		l.advance()
	}

	if l.pos >= len(l.src) {
		return
	}

	ch := l.src[l.pos]
	startLine := l.line
	startCol := l.col

	switch {
	case ch == '\n':
		l.advance()
		l.emit(TokenNewline, "", startLine, startCol)
		l.atLineStart = true

	case ch == '#':
		// Inline comment (`#` or `##`) — consume rest of line
		l.scanComment(startLine)

	case ch == ':':
		l.advance()
		l.emit(TokenColon, ":", startLine, startCol)

	case ch == '-' && l.dashOpensItem():
		l.advance()
		l.emit(TokenDash, "-", startLine, startCol)

	case ch == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '>':
		l.advance()
		l.advance()
		l.emit(TokenArrow, "->", startLine, startCol)

	case ch == '=':
		l.advance()
		l.emit(TokenEquals, "=", startLine, startCol)

	case ch == ',':
		l.advance()
		l.emit(TokenComma, ",", startLine, startCol)

	case ch == '[':
		l.advance()
		l.emit(TokenLBrack, "[", startLine, startCol)

	case ch == ']':
		l.advance()
		l.emit(TokenRBrack, "]", startLine, startCol)

	case ch == '{':
		l.advance()
		l.emit(TokenLBrace, "{", startLine, startCol)

	case ch == '}':
		l.advance()
		l.emit(TokenRBrace, "}", startLine, startCol)

	case ch == '(':
		l.advance()
		l.emit(TokenLParen, "(", startLine, startCol)

	case ch == ')':
		l.advance()
		l.emit(TokenRParen, ")", startLine, startCol)

	case ch == '.':
		l.advance()
		l.emit(TokenDot, ".", startLine, startCol)

	case ch == '*':
		l.advance()
		l.emit(TokenStar, "*", startLine, startCol)

	case ch == '"':
		l.scanString(startLine, startCol)

	case ch == '`':
		l.scanRawString(startLine, startCol)

	case ch == '|':
		// `|` is only meaningful as a block-scalar opener immediately
		// after a `:` (with optional inline whitespace already skipped).
		// Anywhere else it is an error — the DSL has no boolean `|`.
		if l.lastSignificantToken() == TokenColon {
			l.scanBlockScalar(startLine, startCol)
		} else {
			l.advance()
			l.emitError(DiagUnexpectedToken, "unexpected '|': a block scalar opener is only valid right after `key:`", startLine, startCol)
		}

	case unicode.IsDigit(ch):
		l.scanNumber(startLine, startCol)

	case isIdentStart(ch):
		l.scanIdentOrKeyword(startLine, startCol)

	default:
		l.advance()
		msg := fmt.Sprintf("unexpected character %q", string(ch))
		if strings.ContainsRune("&<>!?", ch) {
			// The character of an operator, outside a string: what an
			// author writes when a condition or an expression is left
			// bare — the one shape the language has for it is quoted
			// (measured on the authoring probe, both surfaces).
			msg += ": an operator belongs in a quoted expression (`when \"a && b\"`, `expr: \"x > 1\"`)"
		}
		l.emitError(DiagUnexpectedToken, msg, startLine, startCol)
	}
}

func (l *Lexer) scanString(startLine, startCol int) {
	l.advance() // skip opening "
	var buf []rune
	noted := false // the literal's backslashes are one ProfileRead
	for l.pos < len(l.src) && l.src[l.pos] != '"' {
		if l.src[l.pos] == '\\' && l.pos+1 < len(l.src) {
			if l.strictEscape {
				next := l.src[l.pos+1]
				switch next {
				case '"':
					buf = append(buf, '"')
				case '\\':
					buf = append(buf, '\\')
				case 'n':
					buf = append(buf, '\n')
				case 't':
					buf = append(buf, '\t')
				case 'r':
					buf = append(buf, '\r')
				case '0':
					buf = append(buf, 0)
				default:
					l.emitError(DiagBadEscape, fmt.Sprintf("unknown escape sequence \\%c in strict-escape mode", next), startLine, startCol)
					// Consume the rest of the literal: one bad escape is one
					// diagnostic, not one plus an "unexpected character" for
					// every byte the string still holds.
					l.skipRestOfString()
					return
				}
				l.advance()
				l.advance()
				continue
			}
			// Profile 1 keeps the backslash and the next character verbatim
			// — the one reading profile 2 changes.
			if !noted {
				l.profileReads = append(l.profileReads, ProfileRead{Line: startLine, Kind: "escape"})
				noted = true
			}
			buf = append(buf, l.src[l.pos], l.src[l.pos+1])
			l.advance()
			l.advance()
			continue
		}
		if l.src[l.pos] == '\n' {
			l.emitError(DiagUnterminatedStr, "unterminated string literal", startLine, startCol)
			return
		}
		buf = append(buf, l.src[l.pos])
		l.advance()
	}
	if l.pos < len(l.src) {
		l.advance() // skip closing "
	} else {
		l.emitError(DiagUnterminatedStr, "unterminated string literal", startLine, startCol)
		return
	}
	l.emit(TokenString, string(buf), startLine, startCol)
}

// scanRawString reads a backtick-delimited raw string literal. No
// escape processing is performed — every byte between the opening
// and closing backtick (including newlines, double quotes, and
// backslashes) lands verbatim in the token. Used for recipe content
// that would otherwise drown in `\"`/`\\` escapes: inline shell
// pipelines with embedded JSON, jq filters, Node/Python snippets, …
// To embed a literal backtick, splice another string at concatenation
// time (the lexer offers no escape inside a raw string by design).
func (l *Lexer) scanRawString(startLine, startCol int) {
	l.advance() // skip opening `
	var buf []rune
	for l.pos < len(l.src) && l.src[l.pos] != '`' {
		buf = append(buf, l.src[l.pos])
		l.advance()
	}
	if l.pos >= len(l.src) {
		l.emitError(DiagUnterminatedStr, "unterminated raw string literal (missing closing backtick)", startLine, startCol)
		return
	}
	l.advance() // skip closing `
	l.emit(TokenString, string(buf), startLine, startCol)
}

// lastSignificantToken returns the type of the most recently emitted
// token, skipping over comments. Used by scanToken to decide whether a
// `|` opens a block scalar (only valid right after a `:`).
func (l *Lexer) lastSignificantToken() TokenType {
	for i := len(l.tokens) - 1; i >= 0; i-- {
		if l.tokens[i].Type == TokenComment {
			continue
		}
		return l.tokens[i].Type
	}
	return TokenEOF
}

// scanBlockScalar handles the YAML-style `|` multi-line string opener.
// At call time `|` is the current rune and we know it follows a `:`.
//
//	key: |
//	  line 1
//	  line 2
//	next_key: ...
//
// The indentation of the first non-blank content line defines the strip
// prefix; subsequent lines have that many leading spaces removed and
// the remainder accumulated verbatim (newlines preserved). The block
// ends on the first line less indented than the strip prefix (or EOF).
// One trailing newline is kept (YAML "clip" chomp).
//
// The lexer emits the accumulated content as a single TokenString plus a
// virtual TokenNewline so the parser sees the same shape as
// `key: "..."` followed by a newline.
func (l *Lexer) scanBlockScalar(startLine, startCol int) {
	l.advance() // skip opening |
	// Skip trailing inline whitespace and an optional comment on the
	// opener line, then consume the newline that introduces the block.
	for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t') {
		l.advance()
	}
	if l.pos < len(l.src) && l.src[l.pos] == '#' {
		for l.pos < len(l.src) && l.src[l.pos] != '\n' {
			l.advance()
		}
	}
	if l.pos < len(l.src) && l.src[l.pos] != '\n' {
		l.emitError(DiagExpectedToken, "expected newline after '|' (block scalar opener)", startLine, startCol)
		return
	}
	if l.pos < len(l.src) {
		l.advance() // consume opener-line newline (not emitted as TokenNewline; STRING+NEWLINE come at block end)
	}
	l.blockScalarMode = true
	l.blockScalarBuf = nil
	l.blockScalarBaseLevel = -1
	l.blockScalarStartLine = startLine
	l.blockScalarStartCol = startCol
	l.atLineStart = true
}

// handleBlockScalarLine processes one logical line while the lexer is
// in block-scalar mode. `spaces` is the count of leading spaces already
// consumed by handleLineStart. The function either:
//   - records this line as content (preserving relative indentation),
//   - records a blank line as an empty line in the buffer, or
//   - closes the block (emitting STRING + virtual NEWLINE) and re-enters
//     normal indent handling for the current line.
func (l *Lexer) handleBlockScalarLine(spaces, startLine int) {
	// Blank line (including EOF on a blank line): preserve as empty
	// content if we are already inside the block. Lines before the first
	// content line are ignored entirely.
	if l.pos >= len(l.src) || l.src[l.pos] == '\n' {
		if l.blockScalarBaseLevel != -1 {
			l.blockScalarBuf = append(l.blockScalarBuf, '\n')
		}
		if l.pos < len(l.src) {
			l.advance()
		}
		return
	}

	// First non-blank line sets the strip prefix.
	if l.blockScalarBaseLevel == -1 {
		l.blockScalarBaseLevel = spaces
	}

	// A line less indented than the strip prefix ends the block. Emit the
	// accumulated content and re-process this line under normal indent rules.
	if spaces < l.blockScalarBaseLevel {
		l.emit(TokenString, string(l.blockScalarBuf), l.blockScalarStartLine, l.blockScalarStartCol)
		l.emit(TokenNewline, "", l.blockScalarStartLine, l.blockScalarStartCol)
		l.blockScalarMode = false
		l.blockScalarBuf = nil
		l.handleIndentation(spaces, startLine)
		return
	}

	// In-block content line: preserve indentation relative to the strip
	// prefix, then copy the rest of the line including its newline.
	rel := spaces - l.blockScalarBaseLevel
	for i := 0; i < rel; i++ {
		l.blockScalarBuf = append(l.blockScalarBuf, ' ')
	}
	for l.pos < len(l.src) && l.src[l.pos] != '\n' {
		l.blockScalarBuf = append(l.blockScalarBuf, l.src[l.pos])
		l.advance()
	}
	if l.pos < len(l.src) {
		l.blockScalarBuf = append(l.blockScalarBuf, '\n')
		l.advance()
	}
}

// handleIndentation emits INDENT/DEDENT tokens for a line whose leading
// spaces have already been counted (used by block-scalar exit to feed
// the dedenting line back through the regular indent state machine).
func (l *Lexer) handleIndentation(spaces, startLine int) {
	// Comment-only line at this indentation: scan it and stay at line start.
	if l.pos < len(l.src) && l.src[l.pos] == '#' && !l.promptBodyOpensHere(spaces) {
		l.scanComment(startLine)
		l.atLineStart = true
		return
	}

	currentLevel := l.indentStack[len(l.indentStack)-1]
	if spaces > currentLevel {
		if len(l.indentStack) >= maxNestingDepth {
			l.emitError(DiagBadIndentation, fmt.Sprintf("maximum nesting depth exceeded (%d levels)", maxNestingDepth), startLine, 1)
			return
		}
		l.indentStack = append(l.indentStack, spaces)
		l.emit(TokenIndent, "", startLine, 1)
	} else if spaces < currentLevel {
		for len(l.indentStack) > 1 && l.indentStack[len(l.indentStack)-1] > spaces {
			l.indentStack = l.indentStack[:len(l.indentStack)-1]
			l.emit(TokenDedent, "", startLine, 1)
		}
		if l.indentStack[len(l.indentStack)-1] != spaces {
			l.emitError(DiagBadIndentation, "indentation does not match any outer level", startLine, 1)
		}
	}
	l.atLineStart = false
}

func (l *Lexer) scanNumber(startLine, startCol int) {
	var buf []rune
	for l.pos < len(l.src) && unicode.IsDigit(l.src[l.pos]) {
		buf = append(buf, l.src[l.pos])
		l.advance()
	}
	if l.pos < len(l.src) && l.src[l.pos] == '.' && l.pos+1 < len(l.src) && unicode.IsDigit(l.src[l.pos+1]) {
		buf = append(buf, l.src[l.pos])
		l.advance()
		for l.pos < len(l.src) && unicode.IsDigit(l.src[l.pos]) {
			buf = append(buf, l.src[l.pos])
			l.advance()
		}
		l.emit(TokenFloat, string(buf), startLine, startCol)
		return
	}
	l.emit(TokenInt, string(buf), startLine, startCol)
}

func (l *Lexer) scanIdentOrKeyword(startLine, startCol int) {
	var buf []rune
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		buf = append(buf, l.src[l.pos])
		l.advance()
	}
	word := string(buf)

	// Handle "string[]"
	if word == "string" && l.pos+1 < len(l.src) && l.src[l.pos] == '[' && l.src[l.pos+1] == ']' {
		l.advance() // [
		l.advance() // ]
		l.emit(TokenTypeStringArray, "string[]", startLine, startCol)
		return
	}

	if tt, ok := keywords[word]; ok {
		l.emit(tt, word, startLine, startCol)
	} else {
		l.emit(TokenIdent, word, startLine, startCol)
	}
}

func (l *Lexer) advance() {
	if l.pos < len(l.src) {
		if l.src[l.pos] == '\n' {
			l.line++
			l.col = 1
		} else {
			l.col++
		}
		l.pos++
	}
}

// spend takes n records off the budget before the lexer makes them — a
// token is one, a comment commentRecords, a blank prompt line profile 1
// notes instead of a token one — and reports whether it may. The record the
// budget has no room for refuses the whole source where that record starts
// (withinText): what a source costs the reader never outgrows the budget,
// however much text follows.
//
// One writer of the token stream spends nothing: the parser's resync of a
// broken list inserts the DEDENTs that close the block loops the list left
// open (spliceDedents). Each loop was opened by an INDENT spent here and
// closes once, so the stream stays under twice the budget.
func (l *Lexer) spend(line, col, n int) bool {
	if l.refused != BoundNone {
		return false
	}
	if n <= l.budget.Left() {
		l.budget.spent += n
		return true
	}
	line, col = l.withinText(line, col)
	msg := ReadBudgetMessage(l.budget.limit)
	if l.budget.unit {
		msg = UnitBudgetMessage(l.budget.limit)
	}
	l.refuseWhole(BoundBudget, msg, readBudgetHint, line, col)
	return false
}

// withinText is line:col moved off the line after the source's final
// newline — where the end of the file and the dedents closing it stand —
// to the end of the source's last line: a refusal names a place the file
// holds.
func (l *Lexer) withinText(line, col int) (int, int) {
	if n := len(l.lineStarts); n >= 2 && line == n && l.lineStarts[n-1] == len(l.src) {
		return n - 1, l.lineStarts[n-1] - l.lineStarts[n-2]
	}
	return line, col
}

// refuseWhole replaces the token stream with the one refusal of the whole
// source for bound, at line:col, its remedy kept for the parser to report
// (hint), and stops the scanner: the records made so far are dropped, so no
// reader of the stream sees a source cut short, and none of them outlives
// the refusal.
func (l *Lexer) refuseWhole(bound Bound, msg, hint string, line, col int) {
	l.tokens = []Token{{Type: TokenError, Code: DiagUnexpectedToken, Value: msg, Line: line, Column: col}}
	l.refused, l.refusedHint = bound, hint
	l.profileReads, l.pendingBlanks, l.blockScalarBuf = nil, nil, nil
	l.pos = len(l.src)
}

// emit records a token at the position its scanner started from; the
// scanner has consumed the token's text by now, so the current position is
// where it ends.
func (l *Lexer) emit(tt TokenType, value string, line, col int) {
	records := 1
	if tt == TokenComment {
		records = commentRecords
	}
	if !l.spend(line, col, records) {
		return
	}
	offset := 0
	if line >= 1 && line <= len(l.lineStarts) {
		offset = l.lineStarts[line-1] + col - 1
	}
	l.tokens = append(l.tokens, Token{Type: tt, Value: value, Line: line, Column: col, Offset: offset, End: l.pos, EndLine: l.endLineOf(line, l.pos)})
}

// endLineOf is the line the token starting on line and ending at the
// exclusive rune index end sits on last. Read off the line table, so a
// value whose TEXT spans lines is measured by the text and one whose VALUE
// does (an escaped "a\nb") is not.
func (l *Lexer) endLineOf(line, end int) int {
	if end <= 0 || line < 1 {
		return line
	}
	last := end - 1
	// A scanner that must look ahead to find its terminator — the block
	// scalar reads the indentation of the line BELOW the block before it
	// knows the block ended — leaves l.pos past the token's own text.
	// Whitespace at the end is never part of a token's text.
	for last > 0 && (l.src[last] == ' ' || l.src[last] == '\t') {
		last--
	}
	for line < len(l.lineStarts) && l.lineStarts[line] <= last {
		line++
	}
	return line
}

// skipRestOfString advances past the remainder of a quoted literal after an
// error inside it — through the closing quote, or to the end of the line —
// so the error is reported once instead of cascading.
func (l *Lexer) skipRestOfString() {
	for l.pos < len(l.src) && l.src[l.pos] != '\n' {
		ch := l.src[l.pos]
		l.advance()
		if ch == '\\' && l.pos < len(l.src) && l.src[l.pos] != '\n' {
			l.advance()
			continue
		}
		if ch == '"' {
			return
		}
	}
}

// emitError records a lexer diagnosis as an error token that carries its
// own diagnostic code, so the parser can report the cause wherever the token
// surfaces — inside a block as well as at the top level.
func (l *Lexer) emitError(code DiagCode, msg string, line, col int) {
	if !l.spend(line, col, 1) {
		return
	}
	// The diagnosis token stands on the rune it refuses: its extent is that
	// rune, so a span closed on it ends past the rune and not past the text
	// of the diagnosis.
	t := Token{Type: TokenError, Code: code, Value: msg, Line: line, Column: col}
	if line >= 1 && line <= len(l.lineStarts) {
		if off := l.lineStarts[line-1] + col - 1; off >= 0 && off < len(l.src) {
			t.Offset, t.End = off, off+1
		}
	}
	l.tokens = append(l.tokens, t)
}

func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentPart(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
