package parser

import (
	"strconv"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// Codemode sub-grammar — the `codemode:` declaration in both of its forms
// (ADR-165). Carved out beside parser_sandbox.go, whose grammar this one
// copies: `sandbox:` is the precedent for "the short form names a mode, the
// block form is the detail", and a second spelling of that shape is how two
// properties that look alike start behaving differently.

// parseCodemodeBlock handles both forms of the `codemode:` declaration:
//
//	codemode: on                    # short form (on / off)
//	codemode:                       # block form
//	  tool_calls: 256
//	  wall: "2m"
//	  memory: 536870912
//
// The short form is folded into the block struct with Mode set and every
// other field zero, as parseSandboxBlock does.
//
// The block form DEFAULTS Mode to "on" when it declares no `mode:` of its
// own: bounds for a capability that is off express nothing, so the block
// implies the capability it bounds. An author who means off writes the short
// form — and a `mode: off` written beside a declared bound is kept as
// written, for the IR to report (C348) rather than for the parser to repair.
//
// `codemode` is a KEYWORD token, not a bare ident, so the short-form
// lookahead has to accept keyword tokens too — `isKeywordToken` below. A
// reader that only accepted TokenIdent would take `codemode: on` for a block
// header and then fail on the dedent.
func (p *parser) parseCodemodeBlock(host string) *ast.CodemodeBlock {
	defer p.enterBlock(host)()
	start := p.next() // consume "codemode"
	colon, _ := p.expect(TokenColon)

	cb := &ast.CodemodeBlock{Span: ast.Span{Start: p.pos(start)}}

	// Short form: a single word immediately follows the colon, on the same
	// logical line. The compiler, not the parser, narrows it to on/off
	// (C337) — the parser's job is to read the word the author wrote.
	t := p.peek()
	if t.Type == TokenIdent || isKeywordToken(t.Type) {
		cb.Mode = p.expectIdent()
		return cb
	}

	// Block form.
	switch p.blockBodyAfter(colon) {
	case headerFailed:
		return cb // reported; the node still parses
	case headerEmpty:
		// A block header with nothing under it. It declares the capability
		// and no bound — the same program as `codemode: on`, and it is read
		// as that rather than refused: an empty block is an author who was
		// about to write a bound.
		cb.Mode = "on"
		return cb
	}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseCodemodeProp(cb, t)
	}
	if cb.Mode == "" {
		// Block form without an explicit mode → the bounds imply on.
		cb.Mode = "on"
	}
	return cb
}

// parseCodemodeProp dispatches one property line inside a `codemode:` block.
//
// Durations are read as STRINGS and kept verbatim; the parse into a
// time.Duration happens at IR compile, where a bad one gets a diagnostic with
// a position instead of a parser error with no vocabulary for units. Byte
// sizes and counts are bare integers — this DSL has no byte-size value form
// and the repository vendors no parser for one.
func (p *parser) parseCodemodeProp(cb *ast.CodemodeBlock, propTok Token) {
	if propTok.Type != TokenIdent && !isKeywordToken(propTok.Type) {
		p.addError(DiagUnexpectedToken, propTok, "unexpected token "+strconv.Quote(propTok.Value)+" in codemode block")
		p.next()
		p.skipToNewline()
		return
	}
	name := propTok.Value
	p.next() // consume the property identifier
	p.expect(TokenColon)

	switch name {
	case "mode":
		cb.Mode = p.expectIdent()
	case "tool_calls":
		cb.ToolCalls = int64(p.expectInt())
	case "wall":
		cb.Wall = p.expectString()
	case "cpu":
		cb.CPU = p.expectString()
	case "memory":
		cb.Memory = int64(p.expectInt())
	case "script_bytes":
		cb.ScriptBytes = int64(p.expectInt())
	case "result_bytes":
		cb.ResultBytes = int64(p.expectInt())
	case "tool_result_bytes":
		cb.ToolResultBytes = int64(p.expectInt())
	default:
		p.unknownProperty("codemode", propTok, name)
		p.skipUnknownProperty()
	}
	p.skipNewlines()
}
