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

// enumWord is the word an enum-valued property names — bare, as every listed
// value is written, or quoted: `session: "fresh"` is `session: fresh`, as
// `backend: "claw"` is `backend: claw`. Every enum reader takes its word
// here, so the two spellings hold for the whole class rather than for the
// one reader that happened to compare values instead of token types. Empty
// when the token is neither a word nor a string; the reader's own
// diagnostic then names the accepted words.
func enumWord(t Token) string {
	if t.Type == TokenString {
		return t.Value
	}
	return tokenAsIdent(t)
}

func (p *parser) parseSessionMode() ast.SessionMode {
	t := p.next()
	switch enumWord(t) {
	case "fresh":
		return ast.SessionFresh
	case "inherit":
		return ast.SessionInherit
	case "inherit_if_available":
		return ast.SessionInheritIfAvailable
	case "artifacts_only":
		return ast.SessionArtifactsOnly
	case "fork":
		return ast.SessionFork
	case "persist":
		return ast.SessionPersist
	default:
		p.addError(DiagInvalidValue, t, "expected session mode (fresh, inherit, inherit_if_available, fork, artifacts_only, persist), got '"+t.Value+"'")
		return ast.SessionFresh
	}
}

// lineEnds reports whether t closes the line a property's colon is on: a
// newline, or a trailing comment — the lexer emits the comment in place of
// the newline it consumes (scanComment), so a `- item` block may follow
// either. Every reader that opens the block after the colon asks this, so
// the two boundaries cannot drift apart again.
func lineEnds(t Token) bool {
	return t.Type == TokenNewline || t.Type == TokenComment
}

// parseNeedsList parses a node's `needs:` value — either a single resource
// name (`needs: godot`) or a bracketed list (`needs: [godot, blender]`).
func (p *parser) parseNeedsList() []string {
	if t := p.peek(); t.Type == TokenLBrack || lineEnds(t) {
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
	if lineEnds(p.peek()) {
		return p.parseDashList(parseElem)
	}
	p.expect(TokenLBrack)
	if p.peek().Type == TokenRBrack {
		p.next()
		return nil
	}
	return p.parseBracketElems(parseElem)
}

// parseBracketElems reads the elements of an inline list whose `[` is already
// consumed and which is known not to be empty, through the closing `]`. It is
// shared with parseDeclaredToolList, the one list whose empty inline form is
// a value rather than an absence.
func (p *parser) parseBracketElems(parseElem func() (value string, ok bool)) []string {
	var out []string
	appendElem := func() {
		first := p.peek()
		if v, ok := parseElem(); ok {
			out = append(out, v)
		} else {
			p.resyncListElement(first)
		}
	}
	appendElem()
	for {
		t := p.peek()
		switch {
		case t.Type == TokenComma:
			p.next()
			if p.peek().Type == TokenRBrack {
				// A trailing comma closes the list, as it does in the JSON
				// value form; handed to the element reader, the `]` was
				// refused as an element and then missed as the closer.
				p.next()
				return out
			}
			appendElem()
		case t.Type == TokenRBrack:
			p.next()
			return out
		case t.Type == TokenEOF || t.Type == TokenDedent || lineEnds(t):
			p.expectFailed(t, TokenRBrack, "expected ] to close the list, got "+t.Type.String())
			return out
		default:
			// Another element with no comma before it, or a stray token:
			// said once, then read as the next element — a stray is refused
			// by the element reader, which consumes it — so the list keeps
			// its shape and nothing runs into the next property. Every
			// iteration consumes at least one token.
			p.addErrorHint(DiagExpectedToken, t, "expected `,` or `]` after a list element, got "+t.Type.String(), "Separate the elements with commas: `[a, b]`.")
			appendElem()
		}
	}
}

// resyncListElement drops what remains of a refused inline element that
// OPENED a bracket, brace or paren — `[[a], bash]` — so the nested text is
// skipped whole and the next element is read; the element reader consumed
// the refused token, so the bracket it opened counts as open. A refused
// token that opened nothing leaves the rest of the list to the list loop:
// the token after it is either the comma, the closer, or another element
// the loop reads — never eaten in silence. The diagnostic is the reader's.
func (p *parser) resyncListElement(refused Token) {
	depth := 0
	if opensBracket(refused) {
		depth = 1
	}
	for depth > 0 {
		t := p.peek()
		switch {
		case t.Type == TokenEOF || t.Type == TokenDedent || lineEnds(t):
			return
		case opensBracket(t):
			depth++
		case t.Type == TokenRBrack || t.Type == TokenRBrace || t.Type == TokenRParen:
			depth--
		}
		p.next()
	}
}

func opensBracket(t Token) bool {
	return t.Type == TokenLBrack || t.Type == TokenLBrace || t.Type == TokenLParen
}

// parseDashList parses the YAML-style form of a list: after the property's
// colon and newline, an indented block of `- elem` lines, one element per
// line, comment lines allowed between them, ending where the indentation
// falls back. It is the form an author with YAML in their fingers writes
// first; it used to draw three diagnostics per line (a stray `-`, a missing
// `[`, an unknown property named after the element).
func (p *parser) parseDashList(parseElem func() (value string, ok bool)) []string {
	p.next() // the newline after `key:`, or the trailing comment that took its place
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
			if n := p.peek(); lineEnds(n) || n.Type == TokenDedent || n.Type == TokenEOF {
				// A bare `-` is not an empty item: said, and the rest of the
				// list still read.
				p.addErrorHint(DiagExpectedToken, n, "expected an element after `-`: a dash with nothing on its line is not an empty item", "Delete the bare `-`, or write the element after it.")
				continue
			}
			v, ok := parseElem()
			if ok {
				out = append(out, v)
			}
			if n := p.peek(); n.Type != TokenNewline && n.Type != TokenComment && n.Type != TokenDedent && n.Type != TokenEOF {
				// Residue after a GOOD element is a mistake of its own;
				// after a refused one it is the same mistake, already
				// said — the reader consumed the token that opened it.
				if ok {
					p.addError(DiagUnexpectedToken, n, "one `- item` per line: nothing may follow the element but a comment")
				}
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
		t := p.next()
		id := tokenAsIdent(t)
		if id == "" {
			p.listElementRefused(t, "a bare name")
		}
		return id, id != ""
	})
}

// listElementRefused says that an element of a list of names is not one —
// a quoted string, a number — instead of leaving it out: a `servers:
// ["forge"]` used to read as an empty list, and the server was never wired,
// without a word. The element is consumed; the rest of the list is read.
func (p *parser) listElementRefused(t Token, want string) {
	hint := "Every element of this list is a name; delete the element or write a name."
	if t.Type == TokenString {
		hint = "Write the name without quotes: a quoted element is a string, and this list holds names."
	}
	p.addErrorHint(DiagExpectedToken, t, "expected "+want+" in the list, got "+t.Type.String(), hint)
}

func (p *parser) parseStringList() []string {
	return p.parseBracketList(func() (string, bool) {
		if t := p.peek(); t.Type != TokenString && tokenAsIdent(t) == "" {
			// A refused element is not an element: without this, the
			// token's text (`1`) was appended as a string beside the
			// diagnostic.
			p.listElementRefused(p.next(), "a string")
			return "", false
		}
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
	return p.parseBracketList(p.refListElem)
}

// parseDeclaredToolList parses an agent/judge `tools:` — the one list whose
// EMPTY inline form is a value rather than an absence. `tools: []` is the
// author saying "this node has no tools"; no `tools:` line at all leaves the
// surface undeclared, which the CLI backends read as "no restriction". The
// two are told apart by nilness from here down, through the ONE predicate
// toolcatalog.ToolsDeclared.
//
// Every other bracket list keeps parseBracketList's nil: `capabilities: []`
// in particular must stay indistinguishable from an absent one, because a nil
// capability list is what makes a node inherit the workflow's.
//
// The `- item` form cannot express an empty list — an indented block with no
// item is a parse error — so it never yields a declared-empty surface.
func (p *parser) parseDeclaredToolList() []string {
	if lineEnds(p.peek()) {
		return p.parseDashList(p.refListElem)
	}
	// Only a `[` that was really read can open a declaration: `expect`
	// consumes the offending token on a mismatch, so `tools: x]` would
	// otherwise land on the `]` arm and salvage a broken line into a binding
	// `tools: []` that the studio then writes back.
	if _, ok := p.expect(TokenLBrack); !ok {
		return p.parseBracketElems(p.refListElem)
	}
	if p.peek().Type == TokenRBrack {
		p.next()
		return []string{}
	}
	// A non-empty bracket from which NOTHING was read is refused loudly.
	// Silence would have to pick a side and both are wrong: nil reads as an
	// absent list — the CLI backend's whole toolset, the inversion #1615 is
	// about — and an empty slice turns `tools: [*]` into "this node has no
	// tools", rewrites the author's line to `tools: []` on the next `fmt`,
	// and raises the bundle's engine floor off a typo. `[]` is the ONE way
	// to declare an empty surface, and it is spelled with nothing between
	// the brackets.
	at := p.peek()
	out := p.parseBracketElems(p.refListElem)
	if out == nil {
		p.addErrorHint(DiagExpectedToken, at,
			"no tool name was read from this list: every element was refused",
			"Write `tools: []` for a node with no tools, or name the tools.")
	}
	return out
}

// parseSkillList parses a `skills: [...]` list. Each element is either a
// quoted string (required for kebab-case names like "changelog-writer", since
// the lexer does not treat '-' as an identifier part) or a bare dotted ident
// (e.g. house_style) — the grammar of a tool list, read by the one list
// reader every list goes through. Empty list [] is allowed.
func (p *parser) parseSkillList() []string {
	return p.parseBracketList(p.refListElem)
}

// refListElem reads one element of a tool or skill list: a quoted literal,
// or a bare dotted reference.
func (p *parser) refListElem() (string, bool) {
	if p.peek().Type == TokenString {
		v := p.next().Value
		return v, v != ""
	}
	name := p.parseToolRef()
	return name, name != ""
}

// parseToolRef parses a single tool reference: IDENT { "." IDENT } or
// IDENT { "." IDENT } "." "*" for MCP server wildcards (e.g. mcp.claude_code.*).
// A token that cannot open a name (a number, a bracket) is refused where it
// stands — the callers read a quoted element before coming here, so a
// string never reaches this point.
func (p *parser) parseToolRef() string {
	t := p.next()
	id := tokenAsIdent(t)
	if id == "" {
		p.listElementRefused(t, "a name — a bare identifier, dotted for a server's tools (mcp.server.*), or a quoted literal —")
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
