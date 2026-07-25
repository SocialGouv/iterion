package parser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// isTemplate reports whether s carries a `{{ ... }}` substitution marker,
// i.e. it is a runtime-resolved template rather than a static literal.
func isTemplate(s string) bool {
	return strings.Contains(s, "{{")
}

// ---- edge ----

// continueDottedRef extends an already-read identifier with any following
// `.ident` segments, yielding a dotted node reference like `r1.check` used to
// address a node instantiated by a group `use ... as <prefix>`. A plain
// single-ident endpoint (the common case) returns unchanged.
func (p *parser) continueDottedRef(head string) string {
	if head == "" {
		return ""
	}
	for p.peek().Type == TokenDot {
		p.next() // consume '.'
		seg := tokenAsIdent(p.next())
		if seg == "" {
			p.addError(DiagExpectedToken, p.peek(), "expected identifier after '.' in node reference")
			break
		}
		head += "." + seg
	}
	return head
}

func (p *parser) parseEdge() *ast.Edge {
	fromT := p.next()
	from := p.continueDottedRef(tokenAsIdent(fromT))
	if from == "" {
		p.addError(DiagExpectedToken, fromT, "expected source node name in edge")
		p.skipToNewline()
		return nil
	}

	if _, ok := p.expect(TokenArrow); !ok {
		p.skipToNewline()
		return nil
	}

	toT := p.next()
	to := p.continueDottedRef(tokenAsIdent(toT))
	if to == "" {
		p.addError(DiagExpectedToken, toT, "expected target node name in edge")
		p.skipToNewline()
		return nil
	}

	edge := &ast.Edge{
		From: from,
		To:   to,
		Span: ast.Span{Start: p.pos(fromT)},
	}

	// Optional clauses: when|else, as, with (in any order before
	// newline). Reject duplicates — `... when foo when not bar` used to
	// accept the line with the second clause silently overwriting the
	// first. Track each by token kind so the error message points the
	// operator at the right culprit. `else` and `when` are mutually
	// exclusive: else IS the "no sibling when matched" clause, so a
	// guard on top of it is a contradiction.
	var sawWhen, sawElse, sawAs, sawWith bool
	for {
		t := p.peek()
		switch t.Type {
		case TokenWhen:
			if sawWhen {
				p.addError(DiagDuplicateEdgeClause, t, "duplicate 'when' clause on edge")
			}
			if sawElse {
				p.addError(DiagElseWithWhen, t, "edge cannot carry both 'else' and 'when'")
			}
			parsed := p.parseWhenClause()
			if !sawWhen {
				edge.When = parsed
			}
			sawWhen = true
		case TokenElse:
			if sawElse {
				p.addError(DiagDuplicateEdgeClause, t, "duplicate 'else' clause on edge")
			}
			if sawWhen {
				p.addError(DiagElseWithWhen, t, "edge cannot carry both 'when' and 'else'")
			}
			p.next() // consume "else"
			edge.IsElse = true
			sawElse = true
		case TokenAs:
			if sawAs {
				p.addError(DiagDuplicateEdgeClause, t, "duplicate 'as' clause on edge")
			}
			// Disambiguate `as foreach <name>(item in coll)` from the loop form
			// `as <loop_name>(N)`. The lexer has only 1-token lookahead, so we
			// consume `as`, peek the next token, and backup for parseLoopClause
			// (which re-consumes `as`) when it isn't `foreach`.
			p.next() // consume "as" tentatively
			if p.peek().Type == TokenIdent && p.peek().Value == "foreach" {
				if fc := p.parseForeachClause(); !sawAs { // consumes "foreach" onward
					edge.Foreach = fc
				}
			} else {
				p.backup() // restore "as" for parseLoopClause
				if parsed := p.parseLoopClause(); !sawAs {
					edge.Loop = parsed
				}
			}
			sawAs = true
		case TokenWith:
			if sawWith {
				p.addError(DiagDuplicateEdgeClause, t, "duplicate 'with' clause on edge")
			}
			parsed := p.parseWithBlock()
			if !sawWith {
				edge.With = parsed
			}
			sawWith = true
		default:
			goto done
		}
	}
done:
	p.skipNewlines()
	return edge
}

// parseWhenClause parses a `when ...` edge clause. Two forms:
//
//	when [not] <ident>            simple boolean field check (legacy)
//	when "<expression>"           arbitrary boolean expression (quoted)
//
// The expression form must be a single string literal containing the full
// expression source (operators like `&&`, `||`, `==` are not tokenized by
// the iterion lexer, so quoting keeps the surface area small).
func (p *parser) parseWhenClause() *ast.WhenClause {
	start := p.next() // consume "when"
	wc := &ast.WhenClause{Span: ast.Span{Start: p.pos(start)}}

	// Expression form: when "<expression>"
	if p.peek().Type == TokenString {
		t := p.next()
		wc.Expr = t.Value
		if wc.Expr == "" {
			p.addError(DiagExpectedToken, t, "empty expression in 'when \"...\"'")
		}
		return wc
	}

	if p.peek().Type == TokenNot {
		p.next()
		wc.Negated = true
	}

	t := p.next()
	cond := tokenAsIdent(t)
	if cond == "" {
		p.addError(DiagExpectedToken, t, "expected condition identifier or quoted expression after 'when'")
	}
	wc.Condition = cond
	return wc
}

// parseForeachClause parses `as foreach <name>(<item> in <collection>)`. The
// `as` has already been consumed by the caller; this consumes `foreach` onward.
//
//	as foreach scan(item in "{{outputs.list.items}}")
func (p *parser) parseForeachClause() *ast.ForeachClause {
	start := p.next() // consume "foreach"
	fc := &ast.ForeachClause{Span: ast.Span{Start: p.pos(start)}}
	fc.Name = tokenAsIdent(p.next())
	if fc.Name == "" {
		p.addError(DiagExpectedToken, p.peek(), "expected foreach name after 'as foreach'")
	}
	p.expect(TokenLParen)
	fc.Item = tokenAsIdent(p.next())
	if fc.Item == "" {
		p.addError(DiagExpectedToken, p.peek(), "expected element binding identifier in 'foreach "+fc.Name+"(<item> in ...)'")
	}
	// `in` is a bare identifier (not a keyword) between the item and the collection.
	if in := p.peek(); in.Type == TokenIdent && in.Value == "in" {
		p.next()
	} else {
		p.addError(DiagExpectedToken, in, "expected 'in' after the foreach element binding")
	}
	fc.Collection = p.expectString()
	p.expect(TokenRParen)
	return fc
}

func (p *parser) parseLoopClause() *ast.LoopClause {
	start := p.next() // consume "as"
	lc := &ast.LoopClause{Span: ast.Span{Start: p.pos(start)}}

	t := p.next()
	lc.Name = tokenAsIdent(t)
	if lc.Name == "" {
		p.addError(DiagExpectedToken, t, "expected loop name after 'as'")
	}
	p.expect(TokenLParen)
	// The cap is either a literal int (`as fix_loop(3)`) or a quoted
	// template (`as fix_loop("{{outputs.X.cap}}")`) resolved at runtime.
	// Anything else is an error reported at the offending token; we
	// still consume it to keep the parser advancing past the cap.
	switch nt := p.peek(); {
	case nt.Type == TokenInt:
		lc.MaxIterations = p.expectInt()
	case nt.Type == TokenString:
		// A quoted cap is a runtime template (`as fix("{{vars.cap}}")`) —
		// UNLESS it is a plain integer literal (`as fix("2")`), which is an
		// easy mistake since every template form is quoted. Treat that as the
		// integer it obviously is rather than a template with no refs, which
		// would silently resolve to 0 and skip the loop edge as exhausted.
		s := p.expectString()
		if isTemplate(s) {
			lc.MaxIterationsExpr = s
		} else if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			lc.MaxIterations = n
		} else {
			p.addError(DiagExpectedToken, nt,
				fmt.Sprintf("loop cap %q must be an unquoted integer, a template, or 'unbounded' — a quoted non-numeric string has no template refs and would silently cap the loop at 0", s))
		}
	case tokenAsIdent(nt) == "unbounded":
		// `as <name>(unbounded)` or `as <name>(unbounded <fuel>)`: the loop
		// is not iteration-capped; an optional integer sets a per-loop fuel
		// ceiling (otherwise budget.max_iterations is required — C097).
		p.next() // consume "unbounded"
		lc.Unbounded = true
		if p.peek().Type == TokenInt {
			lc.FuelCap = p.expectInt()
		}
	default:
		p.addError(DiagExpectedToken, nt, "expected integer, template string, or 'unbounded' for loop cap, got "+nt.Type.String())
		p.next()
	}
	p.expect(TokenRParen)
	return lc
}

func (p *parser) parseWithBlock() []*ast.WithEntry {
	p.next() // consume "with"
	p.expect(TokenLBrace)
	p.skipNewlines()

	var entries []*ast.WithEntry
	for {
		t := p.peek()
		if t.Type == TokenRBrace || t.Type == TokenEOF {
			break
		}
		if t.Type == TokenNewline {
			p.next()
			continue
		}
		// Skip indent/dedent tokens inside with blocks
		if t.Type == TokenIndent || t.Type == TokenDedent {
			p.next()
			continue
		}
		we := p.parseWithEntry()
		if we != nil {
			entries = append(entries, we)
		}
	}
	p.expect(TokenRBrace)
	return entries
}

func (p *parser) parseWithEntry() *ast.WithEntry {
	keyT := p.next()
	key := tokenAsIdent(keyT)
	if key == "" {
		p.addError(DiagExpectedToken, keyT, "expected key in with block")
		p.skipToNewline()
		return nil
	}
	p.expect(TokenColon)
	valT := p.next()
	if valT.Type != TokenString {
		p.addError(DiagExpectedToken, valT, "expected string value in with block")
		p.skipToNewline() // recover to avoid cascading mis-parse of the rest of the line
		return nil
	}
	// optional trailing comma
	if p.peek().Type == TokenComma {
		p.next()
	}
	p.skipNewlines()

	return &ast.WithEntry{
		Key:   key,
		Value: valT.Value,
		Span:  ast.Span{Start: p.pos(keyT), End: p.pos(valT)},
	}
}
