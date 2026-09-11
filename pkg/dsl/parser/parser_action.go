package parser

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// Parsing for a tool node's connector-action recipe (ADR-098):
//
//	tool comment:
//	  action: forgejo.issue.comment
//	  connection: forge_main
//	  params:
//	    owner: "{{vars.owner}}"
//	    repo: "{{vars.repo}}"
//	    index: "{{outputs.pick.issue}}"
//	    body: "{{outputs.draft.text}}"
//	  timeout: 30s

// expectActionID reads a dotted operation id (`forgejo.issue.comment`).
//
// The lexer hands it back as separate identifiers around dots rather than one
// token, so it is reassembled here. Accepting a quoted string too is a
// deliberate kindness: an author who writes `action: "forgejo.issue.comment"`
// has written something unambiguous, and refusing it would be pedantry rather
// than a guard.
func (p *parser) expectActionID() string {
	if p.peek().Type == TokenString {
		return p.next().Value
	}
	var b strings.Builder
	first := p.next()
	id := tokenAsIdent(first)
	if id == "" {
		p.expectFailed(first, TokenIdent, "expected an operation id like `connector.resource.verb`, got "+first.Type.String())
		return ""
	}
	b.WriteString(id)
	for p.peek().Type == TokenDot {
		p.next() // consume '.'
		seg := p.next()
		part := tokenAsIdent(seg)
		if part == "" {
			// A trailing dot or a non-identifier segment: report where it is
			// rather than silently producing a truncated id that would then
			// fail as "unknown operation" somewhere far from the cause.
			p.expectFailed(seg, TokenIdent, "expected an identifier after '.' in the operation id, got "+seg.Type.String())
			return b.String()
		}
		b.WriteByte('.')
		b.WriteString(part)
	}
	return b.String()
}

// expectScalarText reads a bare scalar (`30s`, `3`) or a quoted string, and
// renders it as text. Durations and counts are naturally written unquoted,
// and requiring quotes for them would be a papercut in every action node.
//
// `what` names the property for the diagnostic, which is the only thing an
// author can act on when a value is refused.
func (p *parser) expectScalarText(what string) string {
	t := p.peek()
	if t.Type == TokenString {
		return p.next().Value
	}
	// A duration is written `30s`, and the lexer splits it into the number and
	// the unit — so a NUMBER followed by a unit on the same line is one value.
	// Without the join, `timeout: 30s` leaves a stray `s` that the property
	// loop then reads as an unknown tool property, a diagnostic that names the
	// wrong thing entirely.
	var head string
	numeric := false
	switch t.Type {
	case TokenInt, TokenFloat:
		head = p.next().Value
		numeric = true
	default:
		head = tokenAsIdent(t)
		if head == "" {
			p.expectFailed(t, TokenString, "expected a value, got "+t.Type.String())
			p.skipToNewline()
			return ""
		}
		p.next()
	}
	// ONLY a number takes a unit. The join was unconditional, and every
	// `params:` value comes through here: `body: hello world` was silently
	// joined into `helloworld` and sent to the vendor, with a third word then
	// read as the next parameter name. A value the author did not write is
	// exactly what this recipe's refusals exist to prevent, arriving one layer
	// earlier where nothing checks it.
	//
	// ADJACENT, too. `30s` is one value the lexer split at a boundary with no
	// space in it, and that is the whole licence for rejoining: a unit token
	// that starts exactly where the number ended. Joining on "numeric and
	// something follows" left the corruption alive for `body: 2 failures`,
	// which became `2failures` with no diagnostic at all — the two-token case
	// slips past the refusal below, since after the join the line IS ended.
	if numeric && !p.atLineEnd() {
		if u := p.peek(); adjacent(t, u) {
			if unit := tokenAsIdent(u); unit != "" {
				head += unit
				p.next()
			}
		}
	}
	if p.atLineEnd() {
		return head
	}
	// Anything still on the line means the value is not a single bare scalar.
	// Diagnosed here, naming the property and the remedy — leaving it for the
	// property loop reproduces the very confusion the join was added to avoid.
	//
	// "not a single bare word" rather than "more than one word", because the
	// author of `repo: refs/heads/main` sees ONE word: what the lexer split on
	// is punctuation, and a message about word COUNT would describe a mistake
	// they did not make.
	p.addErrorHint(DiagInvalidValue, t,
		"`"+what+"`: a value that is not a single bare word must be quoted",
		"write it as a string: `"+what+": \""+p.restOfLineText(t, head)+"\"`")
	return head
}

// adjacent reports whether b begins exactly where a ended, on the same line —
// i.e. the author wrote them with nothing in between. Columns are 1-based and
// counted in runes; a number token is ASCII, so its byte length is its width.
func adjacent(a, b Token) bool {
	return a.Line == b.Line && b.Column == a.Column+len(a.Value)
}

// restOfLineText consumes what remains of the line and returns the value as
// the author WROTE it, so the diagnostic can show them their own text in the
// form it has to take.
//
// Read from the SOURCE, not rebuilt from the tokens. `refs/heads/main` reaches
// here as five tokens, two of which the lexer typed TokenError — and an error
// token's Value is the lexer's DIAGNOSIS, not the character, so reassembling
// produced `refs unexpected character "/" heads …`. A remedy that silently
// rewrites the value it is telling the author to quote is the same defect as
// the join it replaced.
func (p *parser) restOfLineText(head Token, headText string) string {
	// The tokens still have to be consumed either way: an unconsumed one is
	// read by the property loop as the next property name.
	for !p.atLineEnd() {
		p.next()
	}
	text := p.lex.lineTextFrom(head.Line, head.Column)
	if text == "" {
		return headText
	}
	return text
}

// lineTextFrom returns the source text of `line` from `col` to its end, minus
// a trailing comment and trailing space.
func (l *Lexer) lineTextFrom(line, col int) string {
	cur, start := 1, -1
	for i, r := range l.src {
		if cur == line && start < 0 {
			start = i
		}
		if r == '\n' {
			if start >= 0 {
				return trimLineTail(string(l.src[start+min(col-1, i-start) : i]))
			}
			cur++
		}
	}
	if start >= 0 {
		return trimLineTail(string(l.src[start+min(col-1, len(l.src)-start):]))
	}
	return ""
}

// trimLineTail drops a trailing `#` comment and the space before it. Bounded
// to the form the lexer itself recognises as a comment opener, since a `#`
// with no space in front of it is more likely part of a value (a fragment, a
// colour, an issue reference) than a comment.
func trimLineTail(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, " \t\r")
}

// atLineEnd reports whether the next token ends the current line, so a
// two-token scalar (`30` `s`) is only joined when the unit really follows on
// the same line.
//
// A COMMENT ends the line too, and that is not a nicety: `scanComment`
// consumes the newline itself and emits TokenComment IN ITS PLACE, so there is
// no TokenNewline behind a trailing comment — the reason `skipToNewline` and
// `skipNewlinesFrom` both stop on one. Without this case `timeout: 30s # keep
// it short` was read as a value that is not a single bare word: an E020 on a
// valid line, and then `restOfLineText` consumed the comment and kept going
// into the NEXT line, deleting that property from the node. A trailing comment
// must never change whether a line parses, let alone what it declares.
func (p *parser) atLineEnd() bool {
	switch p.peek().Type {
	case TokenNewline, TokenComment, TokenDedent, TokenEOF:
		return true
	}
	return false
}

// parseActionParamsBlock parses the indented `params:` block into an ORDERED
// list. Order is kept because a `.bot` is read and diffed by humans: a map
// would reshuffle an author's own arguments on every round trip through the
// unparser.
func (p *parser) parseActionParamsBlock() []ast.ActionParam {
	colon, _ := p.expect(TokenColon)
	// A bare `params:` declares an EMPTY block, like every other block since
	// #1067: the studio saves a declaration the moment it is created, so a
	// header with no body yet has a written form. What an empty one MEANS is
	// the compiler's call — here it is an action with no arguments, which the
	// executor refuses at call time if the operation requires any.
	//
	// Through the LANGUAGE's own reader, not a hand-rolled one. This block
	// used to test for an INDENT and `skipToNewline()` otherwise, which reads
	// as defensive and is the opposite: `params:` followed by a sibling
	// property swallowed that property's whole line, and an inline `params: {
	// owner: "acme" }` dropped every argument — both with NO diagnostic, on
	// the one recipe whose promise is that the request is what was declared.
	// blockBodyAfter tells an empty body (a dedent, or the blank line the
	// language uses as the discriminant) from a malformed one, and reports the
	// latter with the indentation hint every other block already gives.
	switch p.blockBodyAfter(colon) {
	case headerFailed:
		// The offending token is already consumed (`expect` takes it either
		// way), so what remains of the line has to go with it: left in place
		// it is read by the property loop as the next property NAME, and the
		// author gets a second diagnostic about a `:` or an `owner` they did
		// not write as a property. One error, at the place the mistake is.
		p.skipToNewline()
		return nil
	case headerEmpty:
		return nil
	}

	var out []ast.ActionParam
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		keyTok := p.next()
		key := tokenAsIdent(keyTok)
		if key == "" {
			p.addError(DiagInvalidValue, keyTok, "expected a parameter name, got "+keyTok.Type.String())
			p.skipToNewline()
			continue
		}
		p.expect(TokenColon)
		out = append(out, ast.ActionParam{
			Key:   key,
			Value: p.expectScalarText(key),
			Span:  ast.Span{Start: p.pos(keyTok)},
		})
		p.skipNewlines()
	}
	return out
}
