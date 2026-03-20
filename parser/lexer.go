package parser

import (
	"fmt"
	"strings"
	"unicode"
)

// Safety limits to prevent DoS from malicious .iter files.
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
}

// NewLexer creates a new Lexer for the given source.
func NewLexer(filename, src string) *Lexer {
	if len(src) > maxSourceSize {
		l := &Lexer{file: filename, line: 1, col: 1}
		l.tokens = []Token{{Type: TokenError, Value: fmt.Sprintf("source file exceeds maximum size (%d bytes > %d)", len(src), maxSourceSize), Line: 1, Column: 1}}
		return l
	}
	l := &Lexer{
		src:         []rune(src),
		file:        filename,
		line:        1,
		col:         1,
		indentStack: []int{0},
		atLineStart: true,
	}
	l.tokenize()
	return l
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

// PeekAt returns the token at offset positions ahead without consuming.
func (l *Lexer) PeekAt(offset int) Token {
	idx := l.ti + offset
	if idx < 0 || idx >= len(l.tokens) {
		return Token{Type: TokenEOF}
	}
	return l.tokens[idx]
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
	if idx >= 0 && l.tokens[idx].Type == TokenIdent {
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

	case ch == '"':
		l.scanString(startLine, startCol)

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
