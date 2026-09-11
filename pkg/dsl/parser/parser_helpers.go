package parser

import (
	"fmt"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// ---- shared helpers ----

func (p *parser) parseReasoningEffort() string {
	p.expect(TokenColon)
	t := p.next()
	// Quoted string form: env-overridable, e.g. "${VIBE_EFFORT:-max}".
	// Stored as-is; resolved + validated at runtime.
	if t.Type == TokenString {
		return t.Value
	}
	value := tokenAsIdent(t)
	switch value {
	case "low", "medium", "high", "xhigh", "max", "ultracode":
		return value
	default:
		p.addError(DiagInvalidValue, t, "expected reasoning effort (low, medium, high, xhigh, max, ultracode) or a quoted env-substituted string, got '"+t.Value+"'")
		return ""
	}
}

func (p *parser) parseSessionMode() ast.SessionMode {
	t := p.next()
	switch t.Type {
	case TokenFresh:
		return ast.SessionFresh
	case TokenInherit:
		return ast.SessionInherit
	case TokenInheritIfAvailable:
		return ast.SessionInheritIfAvailable
	case TokenArtifactsOnly:
		return ast.SessionArtifactsOnly
	case TokenFork:
		return ast.SessionFork
	case TokenPersist:
		return ast.SessionPersist
	default:
		p.addError(DiagInvalidValue, t, "expected session mode (fresh, inherit, inherit_if_available, fork, artifacts_only, persist), got '"+t.Value+"'")
		return ast.SessionFresh
	}
}

// parseNeedsList parses a node's `needs:` value — either a single resource
// name (`needs: godot`) or a bracketed list (`needs: [godot, blender]`).
func (p *parser) parseNeedsList() []string {
	switch p.peek().Type {
	case TokenLBrack, TokenNewline:
		return p.parseIdentList()
	}
	id := p.expectIdent()
	if id == "" {
		return nil
	}
	return []string{id}
}

// parseBracketList parses a list in either of its two written forms — the
// inline `[elem, elem, ...]`, or the YAML-style `- elem` lines indented
// under the property (parseDashList) — calling parseElem for each element.
// parseElem reports ok=false to skip appending (used by parsers that only
// accept well-formed elements). Both forms yield the same list and are
// accepted in every profile: the inline form stays valid, so admitting the
// second changed no text's meaning.
func (p *parser) parseBracketList(parseElem func() (value string, ok bool)) []string {
	if p.peek().Type == TokenNewline {
		return p.parseDashList(parseElem)
	}
	p.expect(TokenLBrack)
	var out []string
	appendElem := func() {
		if v, ok := parseElem(); ok {
			out = append(out, v)
		}
	}
	if p.peek().Type == TokenRBrack {
		p.next()
		return out
	}
	appendElem()
	for p.peek().Type == TokenComma {
		p.next() // consume ,
		appendElem()
	}
	p.expect(TokenRBrack)
	return out
}

// parseDashList parses the YAML-style form of a list: after the property's
// colon and newline, an indented block of `- elem` lines, one element per
// line, comment lines allowed between them, ending where the indentation
// falls back. It is the form an author with YAML in their fingers writes
// first; it used to draw three diagnostics per line (a stray `-`, a missing
// `[`, an unknown property named after the element).
func (p *parser) parseDashList(parseElem func() (value string, ok bool)) []string {
	p.next() // the newline after `key:`
	p.skipNewlines()
	if t := p.peek(); t.Type != TokenIndent {
		p.addErrorHint(DiagExpectedToken, t, "expected a list: `[a, b]` after the colon, or `- item` lines indented below the property", "Write `[]` for an empty list.")
		return nil
	}
	p.next() // INDENT
	var out []string
	for {
		t := p.peek()
		switch t.Type {
		case TokenDash:
			p.next()
			if v, ok := parseElem(); ok {
				out = append(out, v)
			}
			if n := p.peek(); n.Type != TokenNewline && n.Type != TokenComment && n.Type != TokenDedent && n.Type != TokenEOF {
				p.addError(DiagUnexpectedToken, n, "one `- item` per line: nothing may follow the element but a comment")
				p.skipToNewline()
			}
		case TokenNewline, TokenComment:
			p.next()
		case TokenDedent:
			p.next()
			return out
		case TokenEOF:
			return out
		case TokenIndent:
			p.addError(DiagBadIndentation, t, "a list item is `- elem` at the list's own indentation; nothing may be indented deeper")
			p.skipIndentedBlock()
		default:
			p.addErrorHint(DiagExpectedToken, t, "expected `- item` in the list, got "+t.Type.String(), "Every line of the list is `- elem`; close the list by outdenting the next property.")
			p.skipToNewline()
		}
	}
}

func (p *parser) parseIdentList() []string {
	return p.parseBracketList(func() (string, bool) {
		id := tokenAsIdent(p.next())
		return id, id != ""
	})
}

func (p *parser) parseStringList() []string {
	return p.parseBracketList(func() (string, bool) {
		return p.expectString(), true
	})
}

// parseToolList parses a bracketed list of tool references that may contain
// dotted qualified names (e.g. [git_diff, mcp.claude_code.delegate]). A
// quoted element is the literal name — the form a name that is not an
// identifier (a kebab-case label from the studio canvas, `"review-ledger"`)
// has to take, and the form the unparser writes for it; without it the
// element was dropped in silence and such a document could never be saved.
func (p *parser) parseToolList() []string {
	return p.parseBracketList(func() (string, bool) {
		if p.peek().Type == TokenString {
			v := p.next().Value
			return v, v != ""
		}
		name := p.parseToolRef()
		return name, name != ""
	})
}

// parseSkillList parses a `skills: [...]` list. Each element is either a
// quoted string (required for kebab-case names like "changelog-writer", since
// the lexer does not treat '-' as an identifier part) or a bare dotted ident
// (e.g. house_style). Empty list [] is allowed.
func (p *parser) parseSkillList() []string {
	if p.peek().Type == TokenNewline {
		return p.parseDashList(func() (string, bool) {
			if p.peek().Type == TokenString {
				v := p.next().Value
				return v, v != ""
			}
			name := p.parseToolRef()
			return name, name != ""
		})
	}
	p.expect(TokenLBrack)
	var names []string
	if p.peek().Type == TokenRBrack {
		p.next()
		return names
	}
	appendRef := func() {
		if p.peek().Type == TokenString {
			names = append(names, p.next().Value)
			return
		}
		if name := p.parseToolRef(); name != "" {
			names = append(names, name)
		}
	}
	appendRef()
	for p.peek().Type == TokenComma {
		p.next() // consume ,
		appendRef()
	}
	p.expect(TokenRBrack)
	return names
}

// parseToolRef parses a single tool reference: IDENT { "." IDENT } or
// IDENT { "." IDENT } "." "*" for MCP server wildcards (e.g. mcp.claude_code.*).
func (p *parser) parseToolRef() string {
	t := p.next()
	id := tokenAsIdent(t)
	if id == "" {
		return ""
	}
	for p.peek().Type == TokenDot {
		p.next() // consume .
		if p.peek().Type == TokenStar {
			p.next() // consume *
			id += ".*"
			break
		}
		t = p.next()
		part := tokenAsIdent(t)
		if part == "" {
			break
		}
		id += "." + part
	}
	return id
}

// expectString reads a string value: a quoted string, a raw string, a `|`
// block scalar — or a bare word (`backend: claw`, `provider: anthropic`),
// which is the string it spells. The one choke point every string-valued
// property goes through, so the bare form holds for the whole class; a
// value that is not one word (`20m`, `a/b`, `x.y`, `two words`) still
// wants its quotes, and the hint says so.
func (p *parser) expectString() string {
	t := p.next()
	if t.Type == TokenString {
		return t.Value
	}
	if word := tokenAsIdent(t); word != "" {
		return word
	}
	p.expectFailed(t, TokenString, "expected string literal, got "+t.Type.String())
	if t.Type == TokenError {
		return "" // the lexer's diagnosis is not a value
	}
	return t.Value
}

func (p *parser) expectIdent() string {
	t := p.next()
	id := tokenAsIdent(t)
	if id != "" {
		return id
	}
	p.expectFailed(t, TokenIdent, "expected identifier, got "+t.Type.String())
	if t.Type == TokenError {
		return "" // the lexer's diagnosis is not a name
	}
	return t.Value
}

func (p *parser) expectInt() int {
	t := p.next()
	if t.Type == TokenInt {
		v, err := strconv.Atoi(t.Value)
		if err != nil {
			p.addError(DiagExpectedToken, t, fmt.Sprintf("invalid integer %q: %v", t.Value, err))
			return 0
		}
		return v
	}
	p.expectFailed(t, TokenInt, "expected integer, got "+t.Type.String())
	return 0
}

func (p *parser) expectNumber() float64 {
	t := p.next()
	switch t.Type {
	case TokenInt, TokenFloat:
		v, err := strconv.ParseFloat(t.Value, 64)
		if err != nil {
			p.addError(DiagExpectedToken, t, fmt.Sprintf("invalid number %q: %v", t.Value, err))
			return 0
		}
		return v
	default:
		p.expectFailed(t, TokenFloat, "expected number, got "+t.Type.String())
		return 0
	}
}

// skipToNewline drops the rest of the current line after an error. A
// trailing comment ends the line too: the lexer emits the comment token in
// place of that line's newline, so running past it would eat the NEXT line
// (a `bogus: 1 # note` used to swallow the `expr:` below it).
func (p *parser) skipToNewline() {
	for {
		t := p.peek()
		if t.Type == TokenNewline || t.Type == TokenComment || t.Type == TokenEOF || t.Type == TokenDedent {
			return
		}
		p.next()
	}
}

// tokenAsIdent returns the identifier string for a token.
// Keywords are also valid as identifiers in name positions.
func tokenAsIdent(t Token) string {
	if t.Type == TokenIdent {
		return t.Value
	}
	// Keywords can be used as identifiers (e.g., node named "input")
	if isKeywordToken(t.Type) {
		return t.Value
	}
	return ""
}

// keywordTokens is every token type the lexer's keyword table produces — the
// set isKeywordToken reads. Derived from the table rather than listed again
// so a keyword added to the lexer is a valid identifier in a name position
// (a node called `emit`, a field called `key`) from the same commit: the
// hand-kept copy of this set drifted twice, refusing a dozen names each
// time (`agent emit:` broke a shipped bot), and read those words as
// "unexpected token" inside the blocks that match properties by value.
var keywordTokens = func() map[TokenType]bool {
	m := make(map[TokenType]bool, len(keywords))
	for _, tt := range keywords {
		m[tt] = true
	}
	return m
}()

// isKeywordToken reports whether tt is a keyword — usable as an identifier
// wherever a name is expected (tokenAsIdent), and a property name where a
// block matches properties by their spelling.
func isKeywordToken(tt TokenType) bool {
	return keywordTokens[tt]
}
