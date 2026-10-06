package parser

import (
	"fmt"
	"slices"
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
	case "none", "low", "medium", "high", "xhigh", "max", "ultracode":
		return value
	default:
		p.addError(DiagInvalidValue, t, "expected reasoning effort (none, low, medium, high, xhigh, max, ultracode) or a quoted env-substituted string, got "+strconv.Quote(t.Value))
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
		p.addError(DiagInvalidValue, t, "expected session mode (fresh, inherit, inherit_if_available, fork, artifacts_only, persist), got "+strconv.Quote(t.Value))
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
	if _, ok := p.expect(TokenLBrack); !ok {
		// The value is not a list: the offending token is consumed and
		// said, and the rest of its line goes with it, so the NEXT property
		// is never read as the list's elements.
		p.skipToNewline()
		return nil
	}
	if p.peek().Type == TokenRBrack {
		p.next()
		return nil
	}
	return p.parseBracketElems(parseElem)
}

// parseBracketElems reads the elements of an inline list whose `[` is already
// consumed, through the closing `]`. It is shared with parseDeclaredToolList,
// the one list whose empty inline form is a value rather than an absence —
// which also reaches it on a `[` that was NOT read, so the first token may
// already end the line; appendElem refuses it there rather than hand it to an
// element reader.
func (p *parser) parseBracketElems(parseElem func() (value string, ok bool)) []string {
	var out []string
	unterminated := func(t Token) bool { return t.Type == TokenEOF || t.Type == TokenDedent || lineEnds(t) }
	// The property whose value the list is, for the resync below: the tokens
	// just consumed are `<name> : [` on both paths into this loop, and the
	// name's column is the indentation a sibling property shares. On
	// parseDeclaredToolList's fall-through path the `[` was NOT read (the
	// offending token stands where it would be), the shape check fails, and
	// the give-up keeps its old recovery — the one the line-end tests pin.
	propTok, propOk := Token{}, false
	if toks, ti := p.lex.tokens, p.lex.ti; ti >= 3 && toks[ti-1].Type == TokenLBrack && toks[ti-2].Type == TokenColon {
		propTok, propOk = toks[ti-3], true
	}
	// appendElem reads one element and reports whether the list goes on. The
	// element reader is never handed the token that ends the line: every
	// element reader consumes at least one token, so it would eat the line
	// end and the loop would read the next property's tokens as elements.
	appendElem := func() bool {
		first := p.peek()
		if unterminated(first) {
			p.expectFailed(first, TokenRBrack, "expected ] to close the list, got "+first.Type.String())
			if propOk && lineEnds(first) {
				p.resyncBrokenBracketList(propTok)
			}
			return false
		}
		if v, ok := parseElem(); ok {
			out = append(out, v)
		} else {
			p.resyncListElement(first)
		}
		return true
	}
	if !appendElem() {
		return out
	}
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
			if !appendElem() {
				return out
			}
		case t.Type == TokenRBrack:
			p.next()
			return out
		case unterminated(t):
			p.expectFailed(t, TokenRBrack, "expected ] to close the list, got "+t.Type.String())
			if propOk && lineEnds(t) {
				p.resyncBrokenBracketList(propTok)
			}
			return out
		default:
			// Another element with no comma before it, or a stray token:
			// said once, then read as the next element — a stray is refused
			// by the element reader, which consumes it — so the list keeps
			// its shape and nothing runs into the next property. Every
			// iteration consumes at least one token.
			p.addErrorHint(DiagExpectedToken, t, "expected `,` or `]` after a list element, got "+t.Type.String(), "Separate the elements with commas: `[a, b]`.")
			if !appendElem() {
				return out
			}
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

// resyncBrokenBracketList is the recovery of an inline list left open at the
// end of its line, the closer — or the rest of the elements — written on a
// line of its own (`tools: [bash,` then `]` below): the first text an author
// with YAML habits writes. The give-up is said where it stands; what would
// otherwise happen is the broken line's DEDENT escaping to the enclosing
// block's property loop, which closes the block on it — the orphaned closer
// then reads as a top-level stray and the property AFTER the list is lost to
// the top-level skip (#1630). The broken list's remainder is everything up
// to the next line that starts at the list's own indentation — a sibling
// property — so the remainder is consumed here (a lexer diagnosis it holds
// is still said, the way skipIndentedBlock says one) and the cursor lands on
// the sibling. The list's own indentation is propTok's column; a same-column
// `]` is left for the block loop, which already refuses it in place.
//
// Two less-indented shapes are rescued too (#2081). A line at an ANCESTOR
// block's column — a `rules:` broken under `network:` with `image:`
// following under `sandbox:` — is landed on when the parser still has that
// column open (openBlockColumns), and each block loop between the list and
// the ancestor is handed the one closing DEDENT it waits for, spliced ahead
// of the ancestor line: the broken remainder's own dedents cannot be trusted
// for the count, because a closer dedented to column 1 over-pops every
// level. And an off-stack dedent in the remainder is read past rather than
// bailing: the misaligned line leaves no level behind (the lexer says E003
// and pushes nothing back, so no DEDENT ever comes at the next outdented
// line), but the closing DEDENTs the open loops wait for are spliced at the
// point the loops close (listBlockClosePoint), one per loop the natural
// pops there do not cover — the same accounting a closer dedented to column
// 1 needs, since it over-pops every level below the list's own. Every
// declaration after the block then still reads as itself.
//
// What still bails, consuming nothing: a remainder that runs out of file,
// reaches the top level, reaches a line whose column no open block owns, or
// holds an off-stack dedent whose close point is unsafe to compute — the
// ordinary recovery needs the dedents as they are there.
func (p *parser) resyncBrokenBracketList(propTok Token) {
	i := 0
	lineStart := false
	dedent := false   // a DEDENT since the last line end
	offStack := false // an off-stack dedent (Error after pops) in the span
	for {
		t := p.lex.PeekAt(i)
		switch {
		case t.Type == TokenEOF:
			return
		case lineEnds(t):
			lineStart, dedent = true, false
			i++
		case t.Type == TokenIndent:
			i++
		case t.Type == TokenDedent:
			dedent = true
			i++
		case t.Type == TokenError:
			// A lexer diagnosis — a tab-indented line, an off-stack dedent, a
			// mid-line one — is not the line's content, and it is said when
			// the span is consumed below. An off-stack dedent (an Error after
			// pops) is safe to read past where the landing arms allow it: the
			// closing DEDENTs its level-loss denied the open loops are
			// spliced at the close point. Read past, or the sibling the
			// diagnosis precedes is never found.
			if dedent {
				offStack = true
			}
			i++
		case lineStart && t.Column == propTok.Column:
			// A sibling of the broken list. When the remainder cannot be
			// trusted for the closing DEDENT count — more than the list's own
			// block open below the top level (a closer dedented to column 1
			// over-pops every level), or an off-stack dedent in the span (the
			// misaligned line leaves NO level behind: the lexer says E003 and
			// pushes nothing back, so no DEDENT ever comes at the next
			// outdented line) — the loops that stay open get their closing
			// DEDENTs spliced at the point they close, and every declaration
			// after the block reads as itself.
			cols := p.openBlockColumns()
			matched := len(cols) >= 2 && cols[len(cols)-1] == propTok.Column
			if offStack && !matched {
				// An off-stack dedent with a stack that no longer matches the
				// list's line: keep the bail.
				return
			}
			if matched && (len(cols) > 2 || offStack) {
				if at, n, ok := p.listBlockClosePoint(i, propTok.Column, cols); ok {
					line := p.lex.PeekAt(at).Line
					p.consumeBrokenListRemainder(i)
					p.spliceDedents(p.lex.ti+at-i, n, line)
					return
				} else if offStack {
					return
				}
				// Without an off-stack dedent the plain landing is main's
				// behavior: keep it when the close point is unsafe.
			}
			p.consumeBrokenListRemainder(i)
			return
		case lineStart && t.Column < propTok.Column && tokenAsIdent(t) != "":
			if offStack || declStartsBlock(t) {
				// A line an ancestor rescue must never claim: an off-stack
				// dedent leaves no level for the splice accounting to trust,
				// and a declaration starter (`agent b:` dedented one level
				// too many stands at the ancestor's column) would be eaten
				// whole by the ancestor's property loop — main's bail lets
				// the dedents close the blocks and the top level read it as
				// itself.
				return
			}
			// A less-indented line: an ancestor block's property, or a line
			// no open block owns. Land only on the former — the column is one
			// the parser still has open, and the broken list's own block is
			// the innermost one (its column is propTok's). The loops between
			// the list and the ancestor get their closing DEDENTs from the
			// splice ahead of the ancestor line; the ancestor's own loop and
			// the ones above it from the splice at the point they close, so
			// every declaration after the block still reads as itself.
			cols := p.openBlockColumns()
			idx := slices.Index(cols, t.Column)
			if idx <= 0 || cols[len(cols)-1] != propTok.Column {
				// The top level (idx 0), a stranger's column, or a stack that
				// no longer matches the list's line: keep the bail.
				return
			}
			at, n, ok := p.listBlockClosePoint(i, t.Column, cols[:idx+1])
			if !ok {
				return
			}
			line := p.lex.PeekAt(at).Line
			p.consumeBrokenListRemainder(i)
			// The farther splice goes first so the nearer index stays valid.
			p.spliceDedents(p.lex.ti+at-i, n, line)
			p.spliceDedents(p.lex.ti, len(cols)-1-idx, t.Line)
			return
		default:
			lineStart = false
			i++
		}
	}
}

// consumeBrokenListRemainder eats the n peeked tokens of a broken inline
// list's remainder, saying each lexer diagnosis the span held once, the way
// skipIndentedBlock says one.
func (p *parser) consumeBrokenListRemainder(n int) {
	for ; n > 0; n-- {
		if tok := p.next(); tok.Type == TokenError {
			p.lexerError(tok)
		}
	}
}

// declStartsBlock reports whether the token opens a top-level declaration —
// the parseFile dispatch's keywords that NEVER name a block property: a
// less-indented line starting with one is a declaration standing at the
// wrong indentation, not a property a rescue may claim. The keywords left
// out of this set (vars, presets, secrets, attachments, prompt, schema,
// mcp_server, dsl) double as property and block names, so a line starting
// with one may genuinely be the ancestor's own property.
func declStartsBlock(t Token) bool {
	switch t.Type {
	case TokenWorkflow, TokenAgent, TokenJudge, TokenRouter, TokenHuman,
		TokenTool, TokenCompute, TokenGroup, TokenUse, TokenEmit, TokenWait,
		TokenAwaitAnswers, TokenFail, TokenSubbot, TokenContract, TokenCursor,
		TokenSupervisor:
		return true
	}
	return false
}

// spliceDedents inserts n synthetic DEDENTs ahead of the token at absolute
// stream index at, tagged with that token's line. A block loop closes on any
// DEDENT, so only the count matters: the resync's DEDENT accounting hands
// each open loop the one closing token the broken list's remainder denied
// it.
func (p *parser) spliceDedents(at, n, line int) {
	if n <= 0 {
		return
	}
	dedents := make([]Token, n)
	for k := range dedents {
		dedents[k] = Token{Type: TokenDedent, Line: line, Column: 1}
	}
	p.lex.tokens = slices.Insert(p.lex.tokens, at, dedents...)
}

// listBlockClosePoint scans past a rescued line (at peek index start) for
// the point where the open block loops above it close: the first line that
// starts left of col with a property or declaration of its own, or EOF. It
// returns the peek index of that line's first content token (of the EOF
// token at end of file) and how many closing DEDENTs the resync must splice
// ahead of it: one per open loop the point closes, minus the pops the
// stream already holds there. The pops are counted off the DEDENT run the
// point opens with, the loops off cols — the columns the parser still has
// open at and above the rescued line — plus the levels the rescued block's
// own subtree pushed along the way (their pops sit in the same run, so they
// join both sides of the count). The count is unsafe — ok is false and the
// rescue bails — when a line-start lexer diagnosis stands before the point,
// when the closing line opens a level of its own (an INDENT the splice
// would have to precede, leaving it with no loop to open), when the line
// left of col starts with junk no block owns (a second broken list's
// closer), or when the count goes negative.
func (p *parser) listBlockClosePoint(start, col int, cols []int) (at, dedents int, ok bool) {
	lineStart := false
	lineIndent := false // the line now starting opened with an INDENT
	depth := 0          // levels the rescued block's own subtree pushed since start
	pending := 0        // DEDENTs of the line now starting, not yet attributed
	for j := start; ; j++ {
		t := p.lex.PeekAt(j)
		switch {
		case t.Type == TokenEOF:
			n := len(cols) - 1 + depth - pending
			if n < 0 {
				return 0, 0, false
			}
			return j, n, true
		case lineEnds(t):
			lineStart = true
			lineIndent = false
		case t.Type == TokenIndent:
			lineIndent = true
			depth++
		case t.Type == TokenDedent:
			pending++
		case t.Type == TokenError && lineStart:
			return 0, 0, false
		case lineStart && t.Column < col:
			if lineIndent || tokenAsIdent(t) == "" {
				return 0, 0, false
			}
			loops := 0
			for _, c := range cols[1:] {
				if c > t.Column {
					loops++
				}
			}
			n := loops + depth - pending
			if n < 0 {
				return 0, 0, false
			}
			return j, n, true
		default:
			if lineStart {
				// The line started with pops of the subtree's own levels —
				// a line left of every open level is a case above, so a line
				// that continues here can only have popped the subtree's.
				depth -= pending
			}
			pending = 0
			lineStart = false
		}
	}
}

// openBlockColumns replays the consumed stream's INDENT/DEDENT pairs to list
// the columns at which the parser's open blocks read their lines — the base
// (top level, column 1) first, the innermost block last. It exists for the
// broken-list resync, which must tell a line an ancestor owns from a line no
// open block owns: the tokens carry no level, but the history does. The
// INDENT carries no column, so the level it pushed is read off the first
// real token of the line it opens (resync-spliced DEDENTs may sit between).
// An earlier resync's consumed remainder held INDENT/DEDENT pairs the
// parser's loops never saw, so the replay can drift from the loops a later
// resync still has open — it then bails where it could have landed, never
// the reverse.
func (p *parser) openBlockColumns() []int {
	cols := []int{1}
	toks := p.lex.tokens[:p.lex.ti]
	for i := 0; i < len(toks); i++ {
		switch toks[i].Type {
		case TokenIndent:
			col := 1
			for j := i + 1; j < len(toks); j++ {
				if toks[j].Type != TokenIndent && toks[j].Type != TokenDedent {
					col = toks[j].Column
					break
				}
			}
			cols = append(cols, col)
		case TokenDedent:
			if len(cols) > 1 {
				cols = cols[:len(cols)-1]
			}
		}
	}
	return cols
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
