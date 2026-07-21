package parser

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
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
}

// ---- helpers ----

func (p *parser) peek() Token { return p.lex.Peek() }
func (p *parser) next() Token { return p.lex.Next() }
func (p *parser) backup()     { p.lex.Backup() }

func (p *parser) pos(t Token) ast.Pos {
	return ast.Pos{File: p.file, Line: t.Line, Column: t.Column}
}

func (p *parser) addError(code DiagCode, t Token, msg string) {
	p.diags = append(p.diags, Diagnostic{
		Code:     code,
		Severity: SeverityError,
		Message:  msg,
		File:     p.file,
		Line:     t.Line,
		Column:   t.Column,
	})
}

// expect consumes the next token if it matches tt; otherwise adds a diagnostic.
func (p *parser) expect(tt TokenType) (Token, bool) {
	t := p.next()
	if t.Type == tt {
		return t, true
	}
	p.addError(DiagExpectedToken, t, "expected "+tt.String()+", got "+t.Type.String())
	return t, false
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
		switch t.Type {
		case TokenEOF:
			return
		case TokenVars, TokenPresets, TokenAttachments, TokenSecrets,
			TokenMCPServer, TokenPrompt, TokenSchema, TokenCursor,
			TokenAgent, TokenJudge, TokenRouter, TokenHuman,
			TokenTool, TokenCompute, TokenEmit, TokenWait, TokenGroup, TokenUse, TokenSubbot, TokenWorkflow:
			return
		case TokenDedent:
			p.next()
		default:
			p.next()
		}
	}
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
			return f

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

		case TokenError:
			// The lexer packs its diagnostic message into t.Value (e.g.
			// "source file exceeds maximum size", "maximum nesting depth
			// exceeded"). Surface it directly instead of wrapping it as
			// an opaque "unexpected token 'X' at top level" — that
			// previously hid the actual cause from the operator.
			p.addError(DiagUnexpectedToken, t, t.Value)
			p.next()
			p.skipToNextTopLevel()

		default:
			p.addError(DiagUnexpectedToken, t, "unexpected token '"+t.Value+"' at top level")
			p.next()
			p.skipToNextTopLevel()
		}
	}
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
