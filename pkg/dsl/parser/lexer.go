package parser

import (
	"fmt"
	"strings"
	"unicode"
)

// Safety limits to prevent DoS from malicious .bot files.
const (
	maxSourceSize   = 10 * 1024 * 1024 // 10 MB max file size
	maxNestingDepth = 100              // max indentation nesting levels
)

// Lexer tokenizes an iterion DSL source file with indent-sensitive INDENT/DEDENT tokens.
type Lexer struct {
	src  []rune
	file string // source filename

	pos  int // current index in src
	line int // 1-based
	col  int // 1-based

	tokens []Token // accumulated tokens
	ti     int     // current read index into tokens

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

	// blockScalarMode: when true, lines are accumulated into blockScalarBuf
	// until we see a line less indented than blockScalarBaseLevel. Triggered
	// by `|` immediately following a colon, YAML-style.
	blockScalarMode      bool
	blockScalarBuf       []rune
	blockScalarBaseLevel int // -1 means "use first non-blank content line's indent"
	blockScalarStartLine int
	blockScalarStartCol  int
}

// NewLexer creates a new Lexer for the given source.
func NewLexer(filename, src string) *Lexer {
	if len(src) > maxSourceSize {
		l := &Lexer{file: filename, line: 1, col: 1}
		l.tokens = []Token{{Type: TokenError, Value: fmt.Sprintf("source file exceeds maximum size (%d bytes > %d)", len(src), maxSourceSize), Line: 1, Column: 1}}
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
	src = strings.TrimPrefix(src, "\ufeff")
	src = strings.ReplaceAll(src, "\r\n", "\n")
	l := &Lexer{
		src:          []rune(src),
		file:         filename,
		line:         1,
		col:          1,
		indentStack:  []int{0},
		atLineStart:  true,
		strictEscape: detectStrictEscape(src),
	}
	l.tokenize()
	return l
}

// detectStrictEscape scans the first directives at the top of the file
// (leading `## key: value` comments before any significant token) and
// returns true if `## strict-escape: on` is present. Recipes opt into
// standard `"..."` escape interpretation by adding this directive as
// the first or among the first comment lines.
func detectStrictEscape(src string) bool {
	lines := strings.SplitN(src, "\n", 32)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "##") {
			return false
		}
		body := strings.TrimSpace(strings.TrimPrefix(trimmed, "##"))
		// Accept `strict-escape: on` (with optional surrounding whitespace
		// already trimmed) and a few cosmetic variants.
		if body == "strict-escape: on" || body == "strict-escape:on" || body == "strict-escape = on" {
			return true
		}
	}
	return false
}

// All returns every token produced by the lexer (for debugging).
func (l *Lexer) All() []Token {
	return l.tokens
}

// Next returns the next token.
func (l *Lexer) Next() Token {
	if l.ti >= len(l.tokens) {
		return Token{Type: TokenEOF, Line: l.line, Column: l.col}
	}
	t := l.tokens[l.ti]
	l.ti++
	return t
}

// Peek returns the next token without consuming it.
func (l *Lexer) Peek() Token {
	if l.ti >= len(l.tokens) {
		return Token{Type: TokenEOF, Line: l.line, Column: l.col}
	}
	return l.tokens[l.ti]
}

// Backup unreads the last consumed token.
func (l *Lexer) Backup() {
	if l.ti > 0 {
		l.ti--
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
	// scalar mode (heredocs preserve tab content verbatim) and in
	// prompt body lines (handled below the dispatch).
	if !l.blockScalarMode && !l.promptMode && l.pos < len(l.src) && l.src[l.pos] == '\t' {
		l.emit(TokenError, "tabs are not allowed for indentation; use spaces", startLine, spaces+1)
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
		if l.pos < len(l.src) {
			l.advance() // consume '\n'
		}
		// stay at line start
		return
	}

	// If in prompt mode, emit raw lines until we see less indentation
	if l.promptMode {
		if spaces < l.promptBodyLevel {
			// End prompt mode, fall through to normal indent handling
			l.promptMode = false
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

	// Comment lines at line start
	if l.pos+1 < len(l.src) && l.src[l.pos] == '#' && l.src[l.pos+1] == '#' {
		l.scanComment(startLine)
		l.atLineStart = true
		return
	}

	// Emit INDENT/DEDENT based on indentation change
	currentLevel := l.indentStack[len(l.indentStack)-1]
	if spaces > currentLevel {
		if len(l.indentStack) >= maxNestingDepth {
			l.emit(TokenError, fmt.Sprintf("maximum nesting depth exceeded (%d levels)", maxNestingDepth), startLine, 1)
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
			l.emit(TokenError, "indentation does not match any outer level", startLine, 1)
		}
	}

	l.atLineStart = false
}

// isPromptIndent checks if the last emitted tokens before the INDENT are: prompt IDENT : NEWLINE INDENT
func (l *Lexer) isPromptIndent() bool {
	n := len(l.tokens)
	if n < 4 {
		return false
	}
	// tokens: ..., TokenPrompt, TokenIdent(name), TokenColon, TokenNewline, TokenIndent(just emitted)
	// The INDENT we just emitted is at n-1
	idx := n - 2 // should be Newline
	if idx >= 0 && l.tokens[idx].Type == TokenNewline {
		idx--
	}
	if idx >= 0 && l.tokens[idx].Type == TokenColon {
		idx--
	}
	if idx >= 0 && (l.tokens[idx].Type == TokenIdent || isKeywordToken(l.tokens[idx].Type)) {
		idx--
	}
	if idx >= 0 && l.tokens[idx].Type == TokenPrompt {
		return true
	}
	return false
}

// emitPromptLine captures the rest of the current line as a prompt text line.
func (l *Lexer) emitPromptLine(leadingSpaces int) {
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

func (l *Lexer) scanComment(startLine int) {
	startCol := l.col
	l.advance() // skip first #
	l.advance() // skip second #
	var buf []rune
	for l.pos < len(l.src) && l.src[l.pos] != '\n' {
		buf = append(buf, l.src[l.pos])
		l.advance()
	}
	if l.pos < len(l.src) {
		l.advance() // consume '\n'
	}
	l.emit(TokenComment, strings.TrimSpace(string(buf)), startLine, startCol)
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

	case ch == '#' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '#':
		// Inline comment — consume rest of line
		l.scanComment(startLine)

	case ch == ':':
		l.advance()
		l.emit(TokenColon, ":", startLine, startCol)

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
			l.emit(TokenError, string(ch), startLine, startCol)
		}

	case unicode.IsDigit(ch):
		l.scanNumber(startLine, startCol)

	case isIdentStart(ch):
		l.scanIdentOrKeyword(startLine, startCol)

	default:
		l.advance()
		l.emit(TokenError, string(ch), startLine, startCol)
	}
}

func (l *Lexer) scanString(startLine, startCol int) {
	l.advance() // skip opening "
	var buf []rune
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
					l.emit(TokenError, fmt.Sprintf("unknown escape sequence \\%c in strict-escape mode", next), startLine, startCol)
					return
				}
				l.advance()
				l.advance()
				continue
			}
			buf = append(buf, l.src[l.pos], l.src[l.pos+1])
			l.advance()
			l.advance()
			continue
		}
		if l.src[l.pos] == '\n' {
			l.emit(TokenError, "unterminated string literal", startLine, startCol)
			return
		}
		buf = append(buf, l.src[l.pos])
		l.advance()
	}
	if l.pos < len(l.src) {
		l.advance() // skip closing "
	} else {
		l.emit(TokenError, "unterminated string literal", startLine, startCol)
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
		l.emit(TokenError, "unterminated raw string literal (missing closing backtick)", startLine, startCol)
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
	// Skip trailing inline whitespace and an optional ## comment on the
	// opener line, then consume the newline that introduces the block.
	for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t') {
		l.advance()
	}
	if l.pos+1 < len(l.src) && l.src[l.pos] == '#' && l.src[l.pos+1] == '#' {
		for l.pos < len(l.src) && l.src[l.pos] != '\n' {
			l.advance()
		}
	}
	if l.pos < len(l.src) && l.src[l.pos] != '\n' {
		l.emit(TokenError, "expected newline after '|' (block scalar opener)", startLine, startCol)
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
	if l.pos+1 < len(l.src) && l.src[l.pos] == '#' && l.src[l.pos+1] == '#' {
		l.scanComment(startLine)
		l.atLineStart = true
		return
	}

	currentLevel := l.indentStack[len(l.indentStack)-1]
	if spaces > currentLevel {
		if len(l.indentStack) >= maxNestingDepth {
			l.emit(TokenError, fmt.Sprintf("maximum nesting depth exceeded (%d levels)", maxNestingDepth), startLine, 1)
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
			l.emit(TokenError, "indentation does not match any outer level", startLine, 1)
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

func (l *Lexer) emit(tt TokenType, value string, line, col int) {
	l.tokens = append(l.tokens, Token{Type: tt, Value: value, Line: line, Column: col})
}

func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentPart(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
