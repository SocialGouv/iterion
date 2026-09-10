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
func (p *parser) expectScalarText() string {
	t := p.peek()
	if t.Type == TokenString {
		return p.next().Value
	}
	// A duration is written `30s`, and the lexer splits it into the number and
	// the unit — so whatever leads, a UNIT that follows on the same line is
	// part of the same value. Without the join, `timeout: 30s` leaves a stray
	// `s` that the property loop then reads as an unknown tool property, a
	// diagnostic that names the wrong thing entirely.
	var head string
	switch t.Type {
	case TokenInt, TokenFloat:
		head = p.next().Value
	default:
		head = tokenAsIdent(t)
		if head == "" {
			p.expectFailed(t, TokenString, "expected a value, got "+t.Type.String())
			p.skipToNewline()
			return ""
		}
		p.next()
	}
	if !p.atLineEnd() {
		if unit := tokenAsIdent(p.peek()); unit != "" {
			p.next()
			return head + unit
		}
	}
	return head
}

// atLineEnd reports whether the next token ends the current line, so a
// two-token scalar (`30` `s`) is only joined when the unit really follows on
// the same line.
func (p *parser) atLineEnd() bool {
	switch p.peek().Type {
	case TokenNewline, TokenDedent, TokenEOF:
		return true
	}
	return false
}

// parseActionParamsBlock parses the indented `params:` block into an ORDERED
// list. Order is kept because a `.bot` is read and diffed by humans: a map
// would reshuffle an author's own arguments on every round trip through the
// unparser.
func (p *parser) parseActionParamsBlock() []ast.ActionParam {
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
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
			Value: p.expectScalarText(),
			Span:  ast.Span{Start: p.pos(keyTok)},
		})
		p.skipNewlines()
	}
	return out
}
