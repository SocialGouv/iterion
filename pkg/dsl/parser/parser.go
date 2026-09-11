package parser

import (
	"fmt"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// ParseResult is the output of Parse.
type ParseResult struct {
	File        *ast.File
	Diagnostics []Diagnostic
}

// Parse parses an iterion DSL source file and returns the AST and any diagnostics.
func Parse(filename, src string) *ParseResult {
	p := &parser{
		lex:  NewLexer(filename, src),
		file: filename,
	}
	f := p.parseFile()
	return &ParseResult{File: f, Diagnostics: p.diags}
}

// parser is the recursive-descent parser state.
type parser struct {
	lex   *Lexer
	file  string
	diags []Diagnostic
	// blockHost is the kind whose body the block being parsed sits in — what
	// an "outdent it" remedy must name when the block has several possible
	// hosts (an mcp: block under a workflow is not an agent's). Set by
	// enterBlock, "" at the top level.
	blockHost string
	// inlinePrompts are the prompts written as the text of a referencing
	// property (promptRef), appended to the file's prompts at the end;
	// inlineByHash dedupes them by body.
	inlinePrompts []*ast.PromptDecl
	inlineByHash  map[string]*ast.PromptDecl
}

// ---- helpers ----

func (p *parser) peek() Token { return p.lex.Peek() }
func (p *parser) next() Token { return p.lex.Next() }
func (p *parser) backup()     { p.lex.Backup() }

func (p *parser) pos(t Token) ast.Pos {
	return ast.Pos{File: p.file, Line: t.Line, Column: t.Column}
}

func (p *parser) addError(code DiagCode, t Token, msg string) {
	p.addErrorHint(code, t, msg, "")
}

// addErrorHint records a diagnostic with a site-specific fix line; an empty
// hint falls back to the code's catalogued one.
//
// A diagnostic attributed to a lexer ERROR token is the lexer's diagnosis,
// whatever the parser wanted at that position: the cause is the character
// the lexer refused (a tab, an unclosed quote, a bad escape), and only the
// lexer's message names it. This is the one place every parse site goes
// through, so no site can report "expected X, got Error" or read the
// diagnosis as a property name.
func (p *parser) addErrorHint(code DiagCode, t Token, msg, hint string) {
	if t.Type == TokenError {
		// Unconditional — a lexer diagnosis may share a parser code (the
		// block-scalar opener is E002), and lexerError passes exactly
		// this pair, so the replacement is idempotent for it.
		code, msg, hint = lexerCode(t), t.Value, ""
	}
	if t.Type == TokenIndent && (code == DiagUnexpectedToken || code == DiagUnknownProperty) {
		// "unexpected token ''" / "unknown property ''": an indent token
		// has no text, and what the author sees is a line indented where
		// no member or property can be — at the top level, in a workflow,
		// in a node body alike.
		code, msg, hint = DiagBadIndentation, indentOutsideBlockMsg, ""
	}
	if hint == "" {
		hint = HintFor(code)
	}
	p.diags = append(p.diags, Diagnostic{
		Code:     code,
		Severity: SeverityError,
		Message:  msg,
		File:     p.file,
		Line:     t.Line,
		Column:   t.Column,
		Hint:     hint,
	})
}

// expect consumes the next token if it matches tt; otherwise adds a diagnostic.
func (p *parser) expect(tt TokenType) (Token, bool) {
	t := p.next()
	if t.Type == tt {
		return t, true
	}
	p.expectFailed(t, tt, "expected "+tt.String()+", got "+t.Type.String())
	return t, false
}

// expectFailed reports a token that is not the shape the parser wanted, with
// the remedy for that shape (a lexer error token is reported as the lexer's
// diagnosis by addErrorHint, never as "expected INDENT, got Error" plus a
// hint about opening a block the author did open).
func (p *parser) expectFailed(t Token, want TokenType, msg string) {
	if t.Type == TokenNewline && want != TokenIndent && want != TokenNewline && p.dashBlockAhead() {
		// A `- item` block under a single-valued property: said once, and
		// the block dropped with it, instead of one stray per line.
		p.addErrorHint(DiagExpectedToken, t, msg+" — this property takes a single value, not a `- item` list", expectedTokenHint(want, t.Type))
		p.skipIndentedBlock()
		return
	}
	p.addErrorHint(DiagExpectedToken, t, msg, expectedTokenHint(want, t.Type))
}

// dashBlockAhead reports whether the next tokens open a YAML-style list
// (an indented block whose first line is a `- item`).
func (p *parser) dashBlockAhead() bool {
	return p.peek().Type == TokenIndent && p.lex.PeekAt(1).Type == TokenDash
}

// lexerError surfaces a diagnosis the lexer already made: the error token
// carries its code and message.
func (p *parser) lexerError(t Token) {
	p.addError(lexerCode(t), t, t.Value)
}

// indentOutsideBlockMsg names an indented line where no member or property
// can be. Phrased for both causes: it may be the consequence of a header
// that failed to open (then an error precedes it) or a plain mistake.
const indentOutsideBlockMsg = "indented line where none can be: it is indented deeper than the block it is in, or the header above it did not open (see the error before this one, if any)"

// lexerCode is the diagnostic code an error token carries (E001 when the
// lexer did not classify it).
func lexerCode(t Token) DiagCode {
	if t.Code == "" {
		return DiagUnexpectedToken
	}
	return t.Code
}

// skipNewlines consumes any consecutive newlines and inline comments.
func (p *parser) skipNewlines() {
	for {
		t := p.peek()
		if t.Type == TokenNewline || t.Type == TokenComment {
			p.next()
			continue
		}
		break
	}
}

// skipToNextTopLevel skips tokens until we reach something that looks like a top-level declaration.
//
// The list must stay in sync with parseFile's dispatch table — any
// top-level keyword missing here gets silently consumed by skip after
// an error in an earlier block, masking the user's actual code.
// Previously TokenPresets and TokenAttachments were missing, so an
// error in `vars:` followed by `presets:` / `attachments:` produced
// "vanished" declarations and confusing downstream diagnostics.
func (p *parser) skipToNextTopLevel() {
	for {
		t := p.peek()
		if t.Type == TokenEOF || isTopLevelKeyword(t.Type) {
			return
		}
		p.next()
	}
}

// isTopLevelKeyword reports whether tt opens a top-level declaration —
// the one list parseFile's dispatch table, the error skip above and the
// empty-declaration rule below all read.
func isTopLevelKeyword(tt TokenType) bool {
	switch tt {
	case TokenVars, TokenPresets, TokenAttachments, TokenSecrets,
		TokenMCPServer, TokenPrompt, TokenSchema, TokenCursor, TokenSupervisor,
		TokenAgent, TokenJudge, TokenRouter, TokenHuman,
		TokenTool, TokenCompute, TokenEmit, TokenWait, TokenAwaitAnswers, TokenFail,
		TokenGroup, TokenUse, TokenSubbot, TokenWorkflow, TokenDSL:
		return true
	}
	return false
}

// skipNewlinesFrom is skipNewlines that also reports whether a BLANK line
// separates the header (whose colon sits on line) from what follows. The
// lexer emits no token for a blank line, so it shows as a jump of more
// than one between the lines of consecutive tokens; a comment line is a
// token and is not a blank line.
func (p *parser) skipNewlinesFrom(line int) (blank bool) {
	for {
		t := p.peek()
		if t.Line > line+1 {
			blank = true
		}
		if t.Type == TokenNewline || t.Type == TokenComment {
			line = t.Line
			p.next()
			continue
		}
		return blank
	}
}

// bodyIsEmpty reports, right after a declaration header's colon and the
// newlines that follow it, that NO body follows: the file ends, or a
// blank line and then another top-level declaration. The blank line is
// what tells an empty declaration from a body at the wrong indentation —
// a group's members are themselves top-level keywords (`agent`, `tool`),
// so without it an unindented group body would read as an empty group
// followed by top-level nodes, silently. A header followed by anything
// else is not empty — it is the E002 with the indentation hint, as
// before; an indented comment alone is not a body either.
func (p *parser) bodyIsEmpty(blank bool) bool {
	t := p.peek()
	if t.Type == TokenIndent {
		return false
	}
	return t.Type == TokenEOF || (blank && isTopLevelKeyword(t.Type))
}

// headerState is what parseDeclHeaderOrEmpty found after a header.
type headerState int

const (
	headerFailed headerState = iota // an error was reported; the declaration is discarded
	headerBody                      // an indented body follows
	headerEmpty                     // no body: an empty declaration
)

// parseDeclHeaderOrEmpty is parseDeclHeader for the declarations that may
// be EMPTY — `prompt p:`, `schema s:` and the like with no indented body.
// The studio saves a declaration the moment it is created, before it has
// a field or a line, so the written form has to exist; the unparser
// writes the bare header and this reads it back as the empty declaration.
func (p *parser) parseDeclHeaderOrEmpty(kind string) (start Token, name string, state headerState) {
	start = p.next() // consume the keyword token
	nameT := p.next()
	name = tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected "+kind+" name")
		p.skipToNextTopLevel()
		return start, "", headerFailed
	}
	colon, _ := p.expect(TokenColon)
	if p.bodyIsEmpty(p.skipNewlinesFrom(colon.Line)) {
		return start, name, headerEmpty
	}
	if _, ok := p.expect(TokenIndent); !ok {
		return start, name, headerFailed
	}
	return start, name, headerBody
}

// parseBlockBody consumes the `:` after a block keyword — `budget:`,
// `memory:`, `vars:` and the like — and the newlines after it, and says
// what follows: an indented body (the INDENT is consumed), nothing (the
// EMPTY block: the bare header stands at the end of its parent — a dedent
// or the end of the file — or a blank line separates it from what
// follows), or a mistake (the E002 with the indentation hint, reported
// here). The blank line is the discriminant the empty declarations use; a
// dedent needs none, since a body cannot be less indented than its header.
func (p *parser) parseBlockBody() headerState {
	colon, _ := p.expect(TokenColon)
	return p.blockBodyAfter(colon)
}

// blockBodyAfter is parseBlockBody for a caller that consumed the colon
// itself.
func (p *parser) blockBodyAfter(colon Token) headerState {
	blank := p.skipNewlinesFrom(colon.Line)
	switch t := p.peek(); {
	case t.Type == TokenIndent:
		p.next()
		return headerBody
	case t.Type == TokenDedent || t.Type == TokenEOF || blank:
		return headerEmpty
	}
	p.expect(TokenIndent) // reports the error, with the indentation hint
	return headerFailed
}

// ---- file ----

// isReservedName reports whether name collides with a reserved target
// (done/fail/…). On collision it records a DiagReservedName diagnostic at tok
// naming the offending declaration kind and returns true, so the caller drops
// the decl rather than appending a phantom entry that downstream consumers
// (the JSON marshaller, the unparse path) would surface alongside the error.
func (p *parser) isReservedName(tok Token, name, kind string) bool {
	if ast.ReservedTargets[name] {
		p.addError(DiagReservedName, tok, "cannot use reserved name '"+name+"' as "+kind+" name")
		return true
	}
	return false
}

func (p *parser) parseFile() *ast.File {
	f := &ast.File{}
	startTok := p.peek()
	// declared is set once a declaration has been parsed: the `dsl:`
	// header may only open the file (parseDSLHeader).
	declared := false

	for {
		// Skip newlines but capture top-level comments
		for {
			t := p.peek()
			if t.Type == TokenNewline {
				p.next()
				continue
			}
			if t.Type == TokenComment {
				p.next()
				f.Comments = append(f.Comments, &ast.Comment{
					Text: t.Value,
					Span: ast.Span{Start: p.pos(t), End: p.pos(t)},
				})
				continue
			}
			break
		}
		t := p.peek()

		switch t.Type {
		case TokenEOF:
			f.Span = ast.Span{Start: p.pos(startTok), End: p.pos(t)}
			f.Prompts = append(f.Prompts, p.inlinePrompts...)
			p.refuseDirectiveInProfile(f)
			return f

		case TokenDSL:
			p.parseDSLHeader(f, declared)
			continue

		case TokenVars:
			vb := p.parseVarsBlock()
			if vb != nil {
				if f.Vars != nil {
					p.addError(DiagDuplicateBlock, t, "duplicate 'vars:' block — keeping first declaration")
				} else {
					f.Vars = vb
				}
			}

		case TokenPresets:
			pb := p.parsePresetsBlock()
			if pb != nil {
				if f.Presets != nil {
					p.addError(DiagDuplicateBlock, t, "duplicate 'presets:' block — keeping first declaration")
				} else {
					f.Presets = pb
				}
			}

		case TokenAttachments:
			ab := p.parseAttachmentsBlock()
			if ab != nil {
				if f.Attachments != nil {
					p.addError(DiagDuplicateBlock, t, "duplicate 'attachments:' block — keeping first declaration")
				} else {
					f.Attachments = ab
				}
			}

		case TokenSecrets:
			sb := p.parseSecretsBlock()
			if sb != nil {
				if f.Secrets != nil {
					p.addError(DiagDuplicateBlock, t, "duplicate 'secrets:' block — keeping first declaration")
				} else {
					f.Secrets = sb
				}
			}

		case TokenMCPServer:
			md := p.parseMCPServerDecl()
			if md != nil {
				f.MCPServers = append(f.MCPServers, md)
			}

		case TokenPrompt:
			if pd := p.parsePromptDecl(); pd != nil && !p.isReservedName(t, pd.Name, "prompt") {
				f.Prompts = append(f.Prompts, pd)
			}

		case TokenSchema:
			if sd := p.parseSchemaDecl(); sd != nil && !p.isReservedName(t, sd.Name, "schema") {
				f.Schemas = append(f.Schemas, sd)
			}

		case TokenCursor:
			if cd := p.parseCursorDecl(); cd != nil && !p.isReservedName(t, cd.Name, "cursor") {
				f.Cursors = append(f.Cursors, cd)
			}

		case TokenSupervisor:
			if sd := p.parseSupervisorDecl(); sd != nil && !p.isReservedName(t, sd.Name, "supervisor") {
				f.Supervisors = append(f.Supervisors, sd)
			}

		case TokenAgent:
			if ad := p.parseAgentDecl(); ad != nil && !p.isReservedName(t, ad.Name, "agent") {
				f.Agents = append(f.Agents, ad)
			}

		case TokenJudge:
			if jd := p.parseJudgeDecl(); jd != nil && !p.isReservedName(t, jd.Name, "judge") {
				f.Judges = append(f.Judges, jd)
			}

		case TokenRouter:
			rd := p.parseRouterDecl()
			if rd != nil {
				f.Routers = append(f.Routers, rd)
			}

		case TokenHuman:
			hd := p.parseHumanDecl()
			if hd != nil {
				f.Humans = append(f.Humans, hd)
			}

		case TokenTool:
			td := p.parseToolNodeDecl()
			if td != nil {
				f.Tools = append(f.Tools, td)
			}

		case TokenCompute:
			if cd := p.parseComputeDecl(); cd != nil && !p.isReservedName(t, cd.Name, "compute") {
				f.Computes = append(f.Computes, cd)
			}

		case TokenGroup:
			gd := p.parseGroupDecl()
			if gd != nil {
				f.Groups = append(f.Groups, gd)
			}

		case TokenUse:
			ud := p.parseUseDecl()
			if ud != nil {
				f.Uses = append(f.Uses, ud)
			}

		case TokenEmit:
			if ed := p.parseEmitDecl(); ed != nil && !p.isReservedName(t, ed.Name, "emit") {
				f.Emits = append(f.Emits, ed)
			}

		case TokenWait:
			if wd := p.parseWaitDecl(); wd != nil && !p.isReservedName(t, wd.Name, "wait") {
				f.Waits = append(f.Waits, wd)
			}

		case TokenAwaitAnswers:
			if ad := p.parseAwaitAnswersDecl(); ad != nil && !p.isReservedName(t, ad.Name, "await_answers") {
				f.AwaitAnswers = append(f.AwaitAnswers, ad)
			}

		case TokenFail:
			if fd := p.parseFailDecl(); fd != nil && !p.isReservedName(t, fd.Name, "fail") {
				f.Fails = append(f.Fails, fd)
			}

		case TokenSubbot:
			sd := p.parseSubbotDecl()
			if sd != nil {
				f.Subbots = append(f.Subbots, sd)
			}

		case TokenWorkflow:
			wd := p.parseWorkflowDecl()
			if wd != nil {
				f.Workflows = append(f.Workflows, wd)
			}

		case TokenDedent:
			// Stray dedent at top level — skip
			p.next()

		case TokenIndent:
			// An indented line with no block open above it (the same
			// message addErrorHint substitutes inside a block).
			p.addError(DiagBadIndentation, t, indentOutsideBlockMsg)
			p.next()
			p.skipToNextTopLevel()

		case TokenError:
			// The lexer's own diagnosis (its code and message ride the
			// token), not an opaque "unexpected token 'X' at top level".
			p.lexerError(t)
			p.next()
			p.skipToNextTopLevel()

		default:
			p.addError(DiagUnexpectedToken, t, "unexpected token '"+t.Value+"' at top level")
			p.next()
			p.skipToNextTopLevel()
		}
		declared = true
	}
}

// refuseDirectiveInProfile reports (E042) every strict-escape directive in a
// file of profile 2 or later: the profile reads standard escapes by
// itself, and a directive left behind claims a mode the file no longer
// opts into — the line-33 rule of profile 1 (parser.Preamble) does not
// even apply to it. Reported at the end of the file, once the profile is
// known, at the directive's own position.
func (p *parser) refuseDirectiveInProfile(f *ast.File) {
	if f.EffectiveProfile() <= ast.DefaultProfile {
		return
	}
	for _, c := range f.Comments {
		if !IsStrictEscapeDirective(c.Text) {
			continue
		}
		at := Token{Type: TokenComment, Value: c.Text, Line: c.Span.Start.Line, Column: c.Span.Start.Column}
		p.addError(DiagDirectiveInProfile, at, fmt.Sprintf("the strict-escape directive is profile 1's — profile %d reads standard escapes by default", f.EffectiveProfile()))
	}
}

// parseDSLHeader reads the `dsl: N` header — the syntax profile of the file
// (ADR-098) — which may only be its FIRST declaration. The lexer took the
// profile off the file's first significant line before tokenising, so a
// header anywhere else was not applied (the strings above it were read as
// profile 1), and saying so (E041) beats a line that claims a reading the
// file did not get. A profile this build does not read is refused by name
// (E040): a newer engine may read it, and `requires.iterion` is what keeps
// such a file off an older build.
func (p *parser) parseDSLHeader(f *ast.File, declared bool) {
	t := p.next() // dsl
	if _, ok := p.expect(TokenColon); !ok {
		p.skipToNewline()
		return
	}
	v := p.peek()
	if v.Type == TokenNewline || v.Type == TokenEOF {
		p.addError(DiagUnknownProfile, v, "dsl: takes the syntax profile as a positive integer (`dsl: 2`), got nothing")
		return
	}
	p.next()
	profile := -1
	if v.Type == TokenInt {
		if n, err := strconv.Atoi(v.Value); err == nil && n >= 1 {
			profile = n
		}
	}
	switch {
	case profile < 1:
		p.addError(DiagUnknownProfile, v, "dsl: takes the syntax profile as a positive integer (`dsl: 2`), got '"+v.Value+"'")
	case profile > MaxProfile:
		p.addError(DiagUnknownProfile, v, fmt.Sprintf("unknown dsl profile %d — this build reads profiles 1 to %d", profile, MaxProfile))
	case declared:
		p.addError(DiagMisplacedHeader, t, "dsl: must be the first declaration of the file — everything above it was read as profile 1")
	case f.Profile != 0:
		p.addError(DiagMisplacedHeader, t, "duplicate dsl: header — keeping the first")
	default:
		f.Profile = profile
	}
	p.skipToNewline()
}

// parseDeclHeader consumes the leading `<keyword> <name>:` + indent that
// every top-level declaration (prompt, schema, cursor, agent, judge,
// router, human, tool, compute, workflow) opens with. It returns the
// keyword token (for span tracking), the declared name, and ok=false
// when the header was malformed — in which case error recovery has
// already advanced the cursor (skipToNextTopLevel on missing name; the
// indent miss simply returns).
//
// Behavior must stay byte-identical to the inlined preamble each decl
// method used previously — see git history pre-this-refactor for the
// reference implementation.
func (p *parser) parseDeclHeader(kind string) (start Token, name string, ok bool) {
	start = p.next() // consume the keyword token
	nameT := p.next()
	name = tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected "+kind+" name")
		p.skipToNextTopLevel()
		return start, "", false
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return start, name, false
	}
	return start, name, true
}

// unknownProperty reports a property the kind does not accept, with the
// remedy the registry can name (spec.UnknownPropertyHint): the closest
// accepted names, the block or the enclosing kind the name belongs to, and
// the kind's own list — so the author does not have to open the reference.
// kind is the registry's name for the kind, which is also the word the
// message uses; the conformance test in pkg/dsl/spec holds the two sets of
// names together.
func (p *parser) unknownProperty(kind string, t Token, name string) {
	p.addErrorHint(DiagUnknownProperty, t, "unknown "+kind+" property '"+name+"'", spec.UnknownPropertyHintIn(kind, p.blockHost, name))
}

// skipIndentedBlock drops the indented block that follows a refused header,
// so its lines are not reported one by one as strays of the parent. A
// lexer diagnosis met on the way (a tab, an unclosed quote) is still
// reported: it is the author's mistake, not cascade noise, and dropped with
// the block it would only resurface after the header is fixed.
func (p *parser) skipIndentedBlock() {
	if p.peek().Type != TokenIndent {
		return
	}
	depth := 0
	for {
		t := p.next()
		switch t.Type {
		case TokenIndent:
			depth++
		case TokenDedent:
			depth--
			if depth == 0 {
				return
			}
		case TokenError:
			p.lexerError(t)
		case TokenEOF:
			return
		}
	}
}

// enterBlock records the kind whose body the block about to be parsed sits
// in, for the remedy an unknown property carries, and returns the restore
// for the caller to defer: blocks nest (a network: inside a sandbox: inside
// a workflow), so the previous host comes back when the inner block ends.
func (p *parser) enterBlock(host string) func() {
	prev := p.blockHost
	p.blockHost = host
	return func() { p.blockHost = prev }
}

// skipUnknownProperty drops what an unknown property brought: the rest of
// its line and, when the property was a misspelt block header, the indented
// body under it — which would otherwise spill into the enclosing kind's
// property switch one stray line at a time, each with a diagnostic of its
// own and the compile errors that follow (a `sandbx:` block's `user:` read
// as the agent's prompt reference). Every E012 site recovers through it.
func (p *parser) skipUnknownProperty() {
	p.skipToNewline()
	p.skipNewlines()
	p.skipIndentedBlock()
}

// declHeaderAhead reports whether the tokens at the cursor read as a
// declaration header — `<keyword> <name>:` — without consuming them.
func (p *parser) declHeaderAhead() bool {
	at := p.lex.ti
	kw := p.next()
	name := p.next()
	isHeader := kw.Type != TokenEOF && tokenAsIdent(name) != "" && p.peek().Type == TokenColon
	p.lex.ti = at
	return isHeader
}
