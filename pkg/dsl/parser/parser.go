package parser

import (
	"fmt"
	"strconv"
	"strings"

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
		case TokenVars, TokenPresets, TokenAttachments,
			TokenMCPServer, TokenPrompt, TokenSchema, TokenCursor,
			TokenAgent, TokenJudge, TokenRouter, TokenHuman,
			TokenTool, TokenCompute, TokenWorkflow:
			return
		case TokenDedent:
			p.next()
		default:
			p.next()
		}
	}
}

// consumeBlock skips an entire indented block (INDENT ... DEDENT).
func (p *parser) consumeBlock() {
	depth := 0
	for {
		t := p.next()
		switch t.Type {
		case TokenIndent:
			depth++
		case TokenDedent:
			if depth == 0 {
				return
			}
			depth--
		case TokenEOF:
			return
		}
	}
}

// ---- file ----

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

		case TokenMCPServer:
			md := p.parseMCPServerDecl()
			if md != nil {
				f.MCPServers = append(f.MCPServers, md)
			}

		case TokenPrompt:
			pd := p.parsePromptDecl()
			if pd != nil {
				if ast.ReservedTargets[pd.Name] {
					p.addError(DiagReservedName, t, "cannot use reserved name '"+pd.Name+"' as prompt name")
					// Drop the reserved-name decl rather than appending
					// it: downstream consumers iterating f.Prompts (the
					// JSON marshaller, the unparse path) used to surface
					// a phantom `prompt done:` entry alongside the
					// diagnostic.
				} else {
					f.Prompts = append(f.Prompts, pd)
				}
			}

		case TokenSchema:
			sd := p.parseSchemaDecl()
			if sd != nil {
				if ast.ReservedTargets[sd.Name] {
					p.addError(DiagReservedName, t, "cannot use reserved name '"+sd.Name+"' as schema name")
				} else {
					f.Schemas = append(f.Schemas, sd)
				}
			}

		case TokenCursor:
			cd := p.parseCursorDecl()
			if cd != nil {
				if ast.ReservedTargets[cd.Name] {
					p.addError(DiagReservedName, t, "cannot use reserved name '"+cd.Name+"' as cursor name")
				} else {
					f.Cursors = append(f.Cursors, cd)
				}
			}

		case TokenAgent:
			ad := p.parseAgentDecl()
			if ad != nil {
				if ast.ReservedTargets[ad.Name] {
					p.addError(DiagReservedName, t, "cannot use reserved name '"+ad.Name+"' as agent name")
				} else {
					f.Agents = append(f.Agents, ad)
				}
			}

		case TokenJudge:
			jd := p.parseJudgeDecl()
			if jd != nil {
				if ast.ReservedTargets[jd.Name] {
					p.addError(DiagReservedName, t, "cannot use reserved name '"+jd.Name+"' as judge name")
				} else {
					f.Judges = append(f.Judges, jd)
				}
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
			cd := p.parseComputeDecl()
			if cd != nil {
				if ast.ReservedTargets[cd.Name] {
					p.addError(DiagReservedName, t, "cannot use reserved name '"+cd.Name+"' as compute name")
				} else {
					f.Computes = append(f.Computes, cd)
				}
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

// ---- vars ----

func (p *parser) parseBool() *bool {
	t := p.next()
	switch t.Type {
	case TokenTrue:
		v := true
		return &v
	case TokenFalse:
		v := false
		return &v
	default:
		p.addError(DiagInvalidValue, t, "expected true or false, got '"+t.Value+"'")
		return nil
	}
}

func (p *parser) parseVarsBlock() *ast.VarsBlock {
	start := p.next() // consume "vars"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	vb := &ast.VarsBlock{Span: ast.Span{Start: p.pos(start)}}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		vf := p.parseVarField()
		if vf != nil {
			vb.Fields = append(vb.Fields, vf)
		}
	}
	if len(vb.Fields) > 0 {
		vb.Span.End = vb.Fields[len(vb.Fields)-1].Span.End
	} else {
		vb.Span.End = vb.Span.Start
	}
	return vb
}

func (p *parser) parseVarField() *ast.VarField {
	nameT := p.next()
	if nameT.Type != TokenIdent && !isKeywordToken(nameT.Type) {
		p.addError(DiagExpectedToken, nameT, "expected variable name, got "+nameT.Type.String())
		p.skipToNewline()
		return nil
	}
	name := nameT.Value
	p.expect(TokenColon)
	te := p.parseTypeExpr()

	var def *ast.Literal
	if p.peek().Type == TokenEquals {
		p.next() // consume =
		def = p.parseLiteral()
	}
	p.skipNewlines()

	return &ast.VarField{
		Name:    name,
		Type:    te,
		Default: def,
		Span:    ast.Span{Start: p.pos(nameT), End: p.pos(nameT)},
	}
}

// ---- presets ----

func (p *parser) parsePresetsBlock() *ast.PresetsBlock {
	start := p.next() // consume "presets"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	pb := &ast.PresetsBlock{Span: ast.Span{Start: p.pos(start)}}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		pe := p.parsePresetEntry()
		if pe != nil {
			pb.Entries = append(pb.Entries, pe)
		}
	}
	if len(pb.Entries) > 0 {
		pb.Span.End = pb.Entries[len(pb.Entries)-1].Span.End
	} else {
		pb.Span.End = pb.Span.Start
	}
	return pb
}

func (p *parser) parsePresetEntry() *ast.Preset {
	nameT := p.next()
	if nameT.Type != TokenIdent && !isKeywordToken(nameT.Type) {
		p.addError(DiagExpectedToken, nameT, "expected preset name, got "+nameT.Type.String())
		p.skipToNewline()
		return nil
	}
	pe := &ast.Preset{
		Name: nameT.Value,
		Span: ast.Span{Start: p.pos(nameT), End: p.pos(nameT)},
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return pe
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
		if t.Type != TokenIdent && !isKeywordToken(t.Type) {
			p.addError(DiagUnexpectedToken, t, "expected variable name in preset entry, got "+t.Value)
			p.next()
			p.skipToNewline()
			continue
		}
		keyT := p.next()
		p.expect(TokenColon)
		lit := p.parseLiteral()
		p.skipNewlines()
		pe.Values = append(pe.Values, &ast.PresetValue{
			Key:   keyT.Value,
			Value: lit,
			Span:  ast.Span{Start: p.pos(keyT), End: p.pos(keyT)},
		})
	}
	return pe
}

// ---- attachments ----

func (p *parser) parseAttachmentsBlock() *ast.AttachmentsBlock {
	start := p.next() // consume "attachments"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	ab := &ast.AttachmentsBlock{Span: ast.Span{Start: p.pos(start)}}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		af := p.parseAttachmentField()
		if af != nil {
			ab.Fields = append(ab.Fields, af)
		}
	}
	if len(ab.Fields) > 0 {
		ab.Span.End = ab.Fields[len(ab.Fields)-1].Span.End
	} else {
		ab.Span.End = ab.Span.Start
	}
	return ab
}

func (p *parser) parseAttachmentField() *ast.AttachmentField {
	nameT := p.next()
	if nameT.Type != TokenIdent && !isKeywordToken(nameT.Type) {
		p.addError(DiagExpectedToken, nameT, "expected attachment name, got "+nameT.Type.String())
		p.skipToNewline()
		return nil
	}
	name := nameT.Value
	p.expect(TokenColon)
	at := p.parseAttachmentType()

	af := &ast.AttachmentField{
		Name: name,
		Type: at,
		Span: ast.Span{Start: p.pos(nameT), End: p.pos(nameT)},
	}
	p.skipNewlines()

	// Optional indented sub-block with description, accept_mime, required.
	if p.peek().Type != TokenIndent {
		return af
	}
	p.next() // consume indent
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		// Property name (ident or keyword used as ident).
		if t.Type != TokenIdent && !isKeywordToken(t.Type) {
			p.addError(DiagUnexpectedToken, t, "unexpected token in attachment block: "+t.Value)
			p.next()
			p.skipToNewline()
			continue
		}
		propName := t.Value
		p.next()
		p.expect(TokenColon)
		switch propName {
		case "description":
			af.Description = p.expectString()
		case "accept_mime":
			af.AcceptMIME = p.parseStringList()
		case "required":
			af.Required = p.parseBool()
		default:
			p.addError(DiagUnknownProperty, t, "unknown attachment property '"+propName+"'")
			p.skipToNewline()
		}
		p.skipNewlines()
	}
	return af
}

func (p *parser) parseAttachmentType() ast.AttachmentTypeExpr {
	t := p.next()
	switch t.Type {
	case TokenTypeFile:
		return ast.AttachmentTypeFile
	case TokenTypeImage:
		return ast.AttachmentTypeImage
	default:
		p.addError(DiagInvalidType, t, "expected attachment type (file, image), got '"+t.Value+"'")
		return ast.AttachmentTypeFile
	}
}

func (p *parser) parseTypeExpr() ast.TypeExpr {
	t := p.next()
	switch t.Type {
	case TokenTypeString:
		return ast.TypeString
	case TokenTypeBool:
		return ast.TypeBool
	case TokenTypeInt:
		return ast.TypeInt
	case TokenTypeFloat:
		return ast.TypeFloat
	case TokenTypeJSON:
		return ast.TypeJSON
	case TokenTypeStringArray:
		return ast.TypeStringArray
	default:
		p.addError(DiagInvalidType, t, "expected type (string, bool, int, float, json, string[]), got '"+t.Value+"'")
		return ast.TypeString
	}
}

func (p *parser) parseLiteral() *ast.Literal {
	t := p.next()
	switch t.Type {
	case TokenString:
		return &ast.Literal{Kind: ast.LitString, Raw: `"` + t.Value + `"`, StrVal: t.Value}
	case TokenInt:
		// Check the strconv error explicitly: out-of-range integer
		// literals would otherwise silently clamp to math.MaxInt64 /
		// math.MinInt64 and propagate as legitimate values into vars
		// defaults, budget literals, loop iteration counts, etc.,
		// producing data corruption from authored input.
		v, err := strconv.ParseInt(t.Value, 10, 64)
		if err != nil {
			p.addError(DiagInvalidValue, t, "invalid integer literal '"+t.Value+"': "+err.Error())
		}
		return &ast.Literal{Kind: ast.LitInt, Raw: t.Value, IntVal: v}
	case TokenFloat:
		// Out-of-range float literals would silently become +Inf/-Inf
		// without this error check — a value that round-trips through
		// JSON as `null` and breaks downstream comparisons and budgets.
		v, err := strconv.ParseFloat(t.Value, 64)
		if err != nil {
			p.addError(DiagInvalidValue, t, "invalid float literal '"+t.Value+"': "+err.Error())
		}
		return &ast.Literal{Kind: ast.LitFloat, Raw: t.Value, FloatVal: v}
	case TokenTrue:
		return &ast.Literal{Kind: ast.LitBool, Raw: "true", BoolVal: true}
	case TokenFalse:
		return &ast.Literal{Kind: ast.LitBool, Raw: "false", BoolVal: false}
	default:
		p.addError(DiagExpectedToken, t, "expected literal value, got "+t.Type.String())
		return &ast.Literal{Kind: ast.LitString, Raw: t.Value, StrVal: t.Value}
	}
}

// ---- prompt ----

func (p *parser) parsePromptDecl() *ast.PromptDecl {
	start := p.next() // consume "prompt"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected prompt name")
		p.skipToNextTopLevel()
		return nil
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	// Collect prompt lines. Anything inside the indented block that is
	// not a TokenPromptLine is a structural error — the lexer should
	// only emit prompt-line tokens here. Without a diagnostic the
	// parser silently swallowed the bad token and the resulting prompt
	// was missing content with no signal to the author. Emit once per
	// stray token so the report points at the precise offset.
	var lines []string
	for {
		t := p.peek()
		if t.Type == TokenPromptLine {
			p.next()
			lines = append(lines, t.Value)
		} else if t.Type == TokenDedent {
			p.next()
			break
		} else if t.Type == TokenEOF {
			break
		} else {
			p.addError(DiagUnexpectedToken, t, "unexpected "+t.Type.String()+" in prompt body (expected an indented text line)")
			p.next()
		}
	}

	body := strings.Join(lines, "\n")

	return &ast.PromptDecl{
		Name: name,
		Body: body,
		Span: ast.Span{Start: p.pos(start), End: p.pos(start)},
	}
}

// ---- schema ----

func (p *parser) parseSchemaDecl() *ast.SchemaDecl {
	start := p.next() // consume "schema"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected schema name")
		p.skipToNextTopLevel()
		return nil
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	sd := &ast.SchemaDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
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
		sf := p.parseSchemaField()
		if sf != nil {
			sd.Fields = append(sd.Fields, sf)
		}
	}
	return sd
}

func (p *parser) parseSchemaField() *ast.SchemaField {
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected field name")
		p.skipToNewline()
		return nil
	}
	p.expect(TokenColon)
	ft := p.parseFieldType()

	var enumVals []string
	if p.peek().Type == TokenLBrack {
		enumVals = p.parseEnumConstraint()
	}

	p.skipNewlines()
	return &ast.SchemaField{
		Name:       name,
		Type:       ft,
		EnumValues: enumVals,
		Span:       ast.Span{Start: p.pos(nameT), End: p.pos(nameT)},
	}
}

func (p *parser) parseFieldType() ast.FieldType {
	t := p.next()
	switch t.Type {
	case TokenTypeString:
		return ast.FieldTypeString
	case TokenTypeBool:
		return ast.FieldTypeBool
	case TokenTypeInt:
		return ast.FieldTypeInt
	case TokenTypeFloat:
		return ast.FieldTypeFloat
	case TokenTypeJSON:
		return ast.FieldTypeJSON
	case TokenTypeStringArray:
		return ast.FieldTypeStringArray
	default:
		p.addError(DiagInvalidType, t, "expected field type, got '"+t.Value+"'")
		return ast.FieldTypeString
	}
}

func (p *parser) parseEnumConstraint() []string {
	p.next() // consume [
	p.expect(TokenEnum)
	p.expect(TokenColon)

	var vals []string
	t := p.next()
	if t.Type == TokenString {
		vals = append(vals, t.Value)
	}
	for p.peek().Type == TokenComma {
		p.next() // consume ,
		t = p.next()
		if t.Type == TokenString {
			vals = append(vals, t.Value)
		}
	}
	p.expect(TokenRBrack)
	return vals
}

// ---- cursor ----

// parseCursorDecl parses a top-level `cursor <name>:` declaration.
// A cursor declares either an enum (`values:`) or a numeric band map
// (`bands:`) — IR validation rejects malformed combinations (C085).
// `description:` is an optional free-text annotation.
func (p *parser) parseCursorDecl() *ast.CursorDecl {
	start := p.next() // consume "cursor"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected cursor name")
		p.skipToNextTopLevel()
		return nil
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	cd := &ast.CursorDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start), End: p.pos(start)},
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
		propName := tokenAsIdent(t)
		if propName == "" {
			p.addError(DiagUnexpectedToken, t, "unexpected token in cursor block: "+t.Value)
			p.next()
			p.skipToNewline()
			continue
		}
		p.next() // consume property keyword
		switch propName {
		case "description":
			p.expect(TokenColon)
			cd.Description = p.expectString()
			p.skipNewlines()
		case "values":
			cd.Values = p.parseCursorEnumValues()
		case "bands":
			cd.Bands = p.parseCursorBands()
		default:
			p.addError(DiagUnknownProperty, t, "unknown cursor property '"+propName+"'")
			p.skipToNewline()
		}
	}
	return cd
}

// parseCursorEnumValues parses a `values:` sub-block. Each line is
// `<ident>: "prompt fragment"`. Order is preserved so numeric
// invocations can snap to a position when the cursor is enum-only.
func (p *parser) parseCursorEnumValues() []*ast.CursorEnumValue {
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}
	var out []*ast.CursorEnumValue
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		nameT := p.next()
		name := tokenAsIdent(nameT)
		if name == "" {
			p.addError(DiagExpectedToken, nameT, "expected cursor value name, got "+nameT.Type.String())
			p.skipToNewline()
			continue
		}
		p.expect(TokenColon)
		prompt := p.expectString()
		p.skipNewlines()
		out = append(out, &ast.CursorEnumValue{
			Name:   name,
			Prompt: prompt,
			Span:   ast.Span{Start: p.pos(nameT), End: p.pos(nameT)},
		})
	}
	return out
}

// parseCursorBands parses a `bands:` sub-block. Each line is
// `"<lo>..<hi>": "prompt"`. The range key is stored verbatim and
// parsed by the IR compiler (so a malformed range surfaces a
// pin-pointed diagnostic at compile, not parse).
func (p *parser) parseCursorBands() []*ast.CursorBand {
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}
	var out []*ast.CursorBand
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		keyT := p.next()
		if keyT.Type != TokenString {
			p.addError(DiagExpectedToken, keyT, "expected quoted band range \"lo..hi\", got "+keyT.Type.String())
			p.skipToNewline()
			continue
		}
		p.expect(TokenColon)
		prompt := p.expectString()
		p.skipNewlines()
		out = append(out, &ast.CursorBand{
			Range:  keyT.Value,
			Prompt: prompt,
			Span:   ast.Span{Start: p.pos(keyT), End: p.pos(keyT)},
		})
	}
	return out
}

// parseCursorsBlock parses a `cursors:` block on an agent or judge.
// Reserved keys: `enabled:` (bool toggle). Other keys are cursor
// activation settings; their values are stored verbatim (ident,
// integer, float, or quoted string for `${VAR}` substitution) and
// resolved by the runtime.
func (p *parser) parseCursorsBlock() *ast.CursorBlock {
	start := p.next() // consume "cursors"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	cb := &ast.CursorBlock{
		Enabled: true, // default: an explicit block opts in
		Span:    ast.Span{Start: p.pos(start), End: p.pos(start)},
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
		keyT := p.next()
		key := tokenAsIdent(keyT)
		if key == "" {
			p.addError(DiagExpectedToken, keyT, "expected cursor name or 'enabled', got "+keyT.Type.String())
			p.skipToNewline()
			continue
		}
		p.expect(TokenColon)
		if key == "enabled" {
			if v := p.parseBool(); v != nil {
				cb.Enabled = *v
			}
			p.skipNewlines()
			continue
		}
		val := p.parseCursorSettingValue()
		p.skipNewlines()
		cb.Settings = append(cb.Settings, &ast.CursorSetting{
			Key:   key,
			Value: val,
			Span:  ast.Span{Start: p.pos(keyT), End: p.pos(keyT)},
		})
	}
	return cb
}

// parseCursorSettingValue accepts the four invocation value shapes:
// identifier (enum name), int/float (numeric), or quoted string
// (free-form, lets `${VAR}` env-substitution survive into the IR).
func (p *parser) parseCursorSettingValue() string {
	t := p.next()
	switch t.Type {
	case TokenString:
		return t.Value
	case TokenInt, TokenFloat:
		return t.Value
	}
	if id := tokenAsIdent(t); id != "" {
		return id
	}
	p.addError(DiagExpectedToken, t, "expected cursor value (identifier, number, or quoted string), got "+t.Type.String())
	return t.Value
}

// ---- agent ----

func (p *parser) parseAgentDecl() *ast.AgentDecl {
	start := p.next() // consume "agent"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected agent name")
		p.skipToNextTopLevel()
		return nil
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	ad := &ast.AgentDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
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
		p.parseAgentProp(ad, t)
	}
	return ad
}

func (p *parser) parseAgentProp(ad *ast.AgentDecl, propTok Token) {
	p.next() // consume property keyword
	switch propTok.Type {
	case TokenModel:
		p.expect(TokenColon)
		ad.Model = p.expectString()
	case TokenInput:
		p.expect(TokenColon)
		ad.Input = p.expectIdent()
	case TokenOutput:
		p.expect(TokenColon)
		ad.Output = p.expectIdent()
	case TokenPublish:
		p.expect(TokenColon)
		ad.Publish = p.expectIdent()
	case TokenSystem:
		p.expect(TokenColon)
		ad.System = p.expectIdent()
	case TokenUser:
		p.expect(TokenColon)
		ad.User = p.expectIdent()
	case TokenSession:
		p.expect(TokenColon)
		ad.Session = p.parseSessionMode()
	case TokenTools:
		p.expect(TokenColon)
		ad.Tools = p.parseToolList()
	case TokenToolPolicy:
		p.expect(TokenColon)
		ad.ToolPolicy = p.parseToolList()
	case TokenCapabilities:
		p.expect(TokenColon)
		ad.Capabilities = p.parseToolList()
	case TokenToolMaxSteps:
		p.expect(TokenColon)
		ad.ToolMaxSteps = p.expectInt()
	case TokenMaxTokens:
		p.expect(TokenColon)
		ad.MaxTokens = p.expectInt()
	case TokenReasoningEffort:
		ad.ReasoningEffort = p.parseReasoningEffort()
	case TokenReadonly:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			ad.Readonly = *v
		}
	case TokenMCP:
		p.backup()
		ad.MCP = p.parseMCPConfigBlock()
	case TokenBackend:
		p.expect(TokenColon)
		ad.Backend = p.expectString()
	case TokenProvider:
		p.expect(TokenColon)
		ad.Provider = p.expectString()
	case TokenInteraction:
		p.expect(TokenColon)
		ad.Interaction = p.parseInteractionMode()
	case TokenInteractionPrompt:
		p.expect(TokenColon)
		ad.InteractionPrompt = p.expectIdent()
	case TokenInteractionModel:
		p.expect(TokenColon)
		ad.InteractionModel = p.expectString()
	case TokenAwait:
		p.expect(TokenColon)
		ad.Await = p.parseAwaitMode()
	case TokenCompaction:
		p.backup()
		ad.Compaction = p.parseCompactionBlock()
	case TokenMemory:
		p.backup()
		ad.Memory = p.parseMemoryBlock()
	case TokenSandbox:
		p.backup()
		ad.Sandbox = p.parseSandboxBlock()
	case TokenCursors:
		p.backup()
		ad.Cursors = p.parseCursorsBlock()
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown agent property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

// ---- judge ----

func (p *parser) parseJudgeDecl() *ast.JudgeDecl {
	start := p.next() // consume "judge"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected judge name")
		p.skipToNextTopLevel()
		return nil
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	jd := &ast.JudgeDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
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
		p.parseJudgeProp(jd, t)
	}
	return jd
}

func (p *parser) parseJudgeProp(jd *ast.JudgeDecl, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenModel:
		p.expect(TokenColon)
		jd.Model = p.expectString()
	case TokenInput:
		p.expect(TokenColon)
		jd.Input = p.expectIdent()
	case TokenOutput:
		p.expect(TokenColon)
		jd.Output = p.expectIdent()
	case TokenPublish:
		p.expect(TokenColon)
		jd.Publish = p.expectIdent()
	case TokenSystem:
		p.expect(TokenColon)
		jd.System = p.expectIdent()
	case TokenUser:
		p.expect(TokenColon)
		jd.User = p.expectIdent()
	case TokenSession:
		p.expect(TokenColon)
		jd.Session = p.parseSessionMode()
	case TokenTools:
		p.expect(TokenColon)
		jd.Tools = p.parseToolList()
	case TokenToolPolicy:
		p.expect(TokenColon)
		jd.ToolPolicy = p.parseToolList()
	case TokenCapabilities:
		p.expect(TokenColon)
		jd.Capabilities = p.parseToolList()
	case TokenToolMaxSteps:
		p.expect(TokenColon)
		jd.ToolMaxSteps = p.expectInt()
	case TokenMaxTokens:
		p.expect(TokenColon)
		jd.MaxTokens = p.expectInt()
	case TokenReasoningEffort:
		jd.ReasoningEffort = p.parseReasoningEffort()
	case TokenReadonly:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			jd.Readonly = *v
		}
	case TokenMCP:
		p.backup()
		jd.MCP = p.parseMCPConfigBlock()
	case TokenBackend:
		p.expect(TokenColon)
		jd.Backend = p.expectString()
	case TokenProvider:
		p.expect(TokenColon)
		jd.Provider = p.expectString()
	case TokenInteraction:
		p.expect(TokenColon)
		jd.Interaction = p.parseInteractionMode()
	case TokenInteractionPrompt:
		p.expect(TokenColon)
		jd.InteractionPrompt = p.expectIdent()
	case TokenInteractionModel:
		p.expect(TokenColon)
		jd.InteractionModel = p.expectString()
	case TokenAwait:
		p.expect(TokenColon)
		jd.Await = p.parseAwaitMode()
	case TokenCompaction:
		p.backup()
		jd.Compaction = p.parseCompactionBlock()
	case TokenMemory:
		p.backup()
		jd.Memory = p.parseMemoryBlock()
	case TokenSandbox:
		p.backup()
		jd.Sandbox = p.parseSandboxBlock()
	case TokenCursors:
		p.backup()
		jd.Cursors = p.parseCursorsBlock()
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown judge property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

// ---- router ----

func (p *parser) parseRouterDecl() *ast.RouterDecl {
	start := p.next() // consume "router"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected router name")
		p.skipToNextTopLevel()
		return nil
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	rd := &ast.RouterDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
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
		switch t.Type {
		case TokenMode:
			p.next()
			p.expect(TokenColon)
			rd.Mode = p.parseRouterMode()
		case TokenModel:
			p.next()
			p.expect(TokenColon)
			rd.Model = p.expectString()
		case TokenBackend:
			p.next()
			p.expect(TokenColon)
			rd.Backend = p.expectString()
		case TokenProvider:
			p.next()
			p.expect(TokenColon)
			rd.Provider = p.expectString()
		case TokenSystem:
			p.next()
			p.expect(TokenColon)
			rd.System = p.expectIdent()
		case TokenUser:
			p.next()
			p.expect(TokenColon)
			rd.User = p.expectIdent()
		case TokenMulti:
			p.next()
			p.expect(TokenColon)
			bt := p.next()
			if bt.Type == TokenTrue {
				rd.Multi = true
			} else if bt.Type != TokenFalse {
				p.addError(DiagInvalidValue, bt, "expected true or false for 'multi'")
			}
		case TokenReasoningEffort:
			p.next()
			rd.ReasoningEffort = p.parseReasoningEffort()
		default:
			p.addError(DiagUnknownProperty, t, "unknown router property '"+t.Value+"'")
			p.next()
			p.skipToNewline()
		}
		p.skipNewlines()
	}
	return rd
}

func (p *parser) parseRouterMode() ast.RouterMode {
	t := p.next()
	switch t.Type {
	case TokenFanOutAll:
		return ast.RouterFanOutAll
	case TokenCondition:
		return ast.RouterCondition
	case TokenRoundRobin:
		return ast.RouterRoundRobin
	case TokenLLM:
		return ast.RouterLLM
	default:
		p.addError(DiagInvalidValue, t, "expected router mode (fan_out_all, condition, round_robin, llm), got '"+t.Value+"'")
		return ast.RouterFanOutAll
	}
}

// ---- await (convergence strategy) ----

func (p *parser) parseAwaitMode() ast.AwaitMode {
	t := p.next()
	switch t.Type {
	case TokenWaitAll:
		return ast.AwaitWaitAll
	case TokenBestEffort:
		return ast.AwaitBestEffort
	default:
		p.addError(DiagInvalidValue, t, "expected await mode (wait_all, best_effort), got '"+t.Value+"'")
		return ast.AwaitWaitAll
	}
}

// ---- human ----

func (p *parser) parseHumanDecl() *ast.HumanDecl {
	start := p.next() // consume "human"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected human name")
		p.skipToNextTopLevel()
		return nil
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	hd := &ast.HumanDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
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
		p.parseHumanProp(hd, t)
	}
	return hd
}

func (p *parser) parseHumanProp(hd *ast.HumanDecl, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenInput:
		p.expect(TokenColon)
		hd.Input = p.expectIdent()
	case TokenOutput:
		p.expect(TokenColon)
		hd.Output = p.expectIdent()
	case TokenPublish:
		p.expect(TokenColon)
		hd.Publish = p.expectIdent()
	case TokenInstructions:
		p.expect(TokenColon)
		hd.Instructions = p.expectIdent()
	case TokenInteraction:
		p.expect(TokenColon)
		hd.Interaction = p.parseInteractionMode()
	case TokenInteractionPrompt:
		p.expect(TokenColon)
		hd.InteractionPrompt = p.expectIdent()
	case TokenInteractionModel:
		p.expect(TokenColon)
		hd.InteractionModel = p.expectString()
	case TokenModel:
		p.expect(TokenColon)
		hd.Model = p.expectString()
	case TokenSystem:
		p.expect(TokenColon)
		hd.System = p.expectIdent()
	case TokenAwait:
		p.expect(TokenColon)
		hd.Await = p.parseAwaitMode()
	case TokenIdent:
		if propTok.Value == "min_answers" {
			p.expect(TokenColon)
			hd.MinAnswers = p.expectInt()
		} else {
			p.addError(DiagUnknownProperty, propTok, "unknown human property '"+propTok.Value+"'")
			p.skipToNewline()
		}
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown human property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

func (p *parser) parseInteractionMode() ast.InteractionMode {
	t := p.next()
	switch t.Value {
	case "none":
		return ast.InteractionNone
	case "human":
		return ast.InteractionHuman
	case "llm":
		return ast.InteractionLLM
	case "llm_or_human":
		return ast.InteractionLLMOrHuman
	default:
		p.addError(DiagInvalidValue, t, "expected interaction mode (none, human, llm, llm_or_human), got '"+t.Value+"'")
		return ast.InteractionNone
	}
}

// ---- tool node ----

func (p *parser) parseToolNodeDecl() *ast.ToolNodeDecl {
	start := p.next() // consume "tool"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected tool name")
		p.skipToNextTopLevel()
		return nil
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	td := &ast.ToolNodeDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
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
		p.parseToolNodeProp(td, t)
	}
	return td
}

func (p *parser) parseToolNodeProp(td *ast.ToolNodeDecl, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenCommand:
		p.expect(TokenColon)
		td.Command = p.expectString()
	case TokenScript:
		p.expect(TokenColon)
		td.Script = p.expectString()
	case TokenLanguage:
		p.expect(TokenColon)
		td.Language = p.expectIdent()
	case TokenInput:
		p.expect(TokenColon)
		td.Input = p.expectIdent()
	case TokenOutput:
		p.expect(TokenColon)
		td.Output = p.expectIdent()
	case TokenAwait:
		p.expect(TokenColon)
		td.Await = p.parseAwaitMode()
	case TokenSandbox:
		p.backup()
		td.Sandbox = p.parseSandboxBlock()
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown tool property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

// ---- compute ----

func (p *parser) parseComputeDecl() *ast.ComputeDecl {
	start := p.next() // consume "compute"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected compute name")
		p.skipToNextTopLevel()
		return nil
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	cd := &ast.ComputeDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
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
		p.parseComputeProp(cd, t)
	}
	return cd
}

func (p *parser) parseComputeProp(cd *ast.ComputeDecl, propTok Token) {
	// Most compute properties are plain identifiers (input, output, expr,
	// await). We resolve by token TYPE for the ones that carry dedicated
	// keywords and by token VALUE for the others.
	p.next()
	switch propTok.Type {
	case TokenInput:
		p.expect(TokenColon)
		cd.Input = p.expectIdent()
	case TokenOutput:
		p.expect(TokenColon)
		cd.Output = p.expectIdent()
	case TokenAwait:
		p.expect(TokenColon)
		cd.Await = p.parseAwaitMode()
	case TokenIdent:
		if propTok.Value == "expr" {
			cd.Expr = p.parseComputeExprBlock()
		} else {
			p.addError(DiagUnknownProperty, propTok, "unknown compute property '"+propTok.Value+"'")
			p.skipToNewline()
		}
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown compute property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

// parseComputeExprBlock parses the indented `expr:` block:
//
//	expr:
//	  field_a: "input.x && input.y"
//	  field_b: "vars.n + 1"
func (p *parser) parseComputeExprBlock() []*ast.ComputeExpr {
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	var entries []*ast.ComputeExpr
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		keyT := p.next()
		key := tokenAsIdent(keyT)
		if key == "" {
			p.addError(DiagExpectedToken, keyT, "expected field name in compute expr block")
			p.skipToNewline()
			continue
		}
		p.expect(TokenColon)
		valT := p.next()
		if valT.Type != TokenString {
			p.addError(DiagExpectedToken, valT, "expected string expression in compute expr block")
			p.skipToNewline()
			continue
		}
		entries = append(entries, &ast.ComputeExpr{
			Key:  key,
			Expr: valT.Value,
			Span: ast.Span{Start: p.pos(keyT), End: p.pos(valT)},
		})
		p.skipNewlines()
	}
	return entries
}

// ---- workflow ----

func (p *parser) parseWorkflowDecl() *ast.WorkflowDecl {
	start := p.next() // consume "workflow"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected workflow name")
		p.skipToNextTopLevel()
		return nil
	}
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	wd := &ast.WorkflowDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
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

		switch t.Type {
		case TokenVars:
			wd.Vars = p.parseVarsBlock()

		case TokenAttachments:
			wd.Attachments = p.parseAttachmentsBlock()

		case TokenMCP:
			wd.MCP = p.parseMCPConfigBlock()

		case TokenEntry:
			p.next() // consume "entry"
			p.expect(TokenColon)
			wd.Entry = p.expectIdent()
			p.skipNewlines()

		case TokenBudget:
			wd.Budget = p.parseBudgetBlock()

		case TokenCompaction:
			wd.Compaction = p.parseCompactionBlock()

		case TokenWorktree:
			p.next() // consume "worktree"
			p.expect(TokenColon)
			wd.Worktree = p.expectIdent()
			p.skipNewlines()

		case TokenSandbox:
			wd.Sandbox = p.parseSandboxBlock()
			p.skipNewlines()

		case TokenDefaultBackend:
			p.next() // consume "default_backend"
			p.expect(TokenColon)
			wd.DefaultBackend = p.expectString()
			p.skipNewlines()

		case TokenToolPolicy:
			p.next() // consume "tool_policy"
			p.expect(TokenColon)
			wd.ToolPolicy = p.parseToolList()
			p.skipNewlines()

		case TokenCapabilities:
			p.next() // consume "capabilities"
			p.expect(TokenColon)
			wd.Capabilities = p.parseToolList()
			p.skipNewlines()

		case TokenInteraction:
			p.next() // consume "interaction"
			p.expect(TokenColon)
			im := p.parseInteractionMode()
			wd.Interaction = &im
			p.skipNewlines()

		case TokenComment:
			p.next() // skip workflow-level comments

		default:
			// Must be an edge: IDENT -> IDENT ...
			if t.Type == TokenIdent || isKeywordToken(t.Type) {
				edge := p.parseEdge()
				if edge != nil {
					wd.Edges = append(wd.Edges, edge)
				}
			} else {
				p.addError(DiagUnexpectedToken, t, "unexpected token '"+t.Value+"' in workflow")
				p.next()
			}
		}
	}
	return wd
}

func (p *parser) parseBudgetBlock() *ast.BudgetBlock {
	start := p.next() // consume "budget"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	bb := &ast.BudgetBlock{Span: ast.Span{Start: p.pos(start)}}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseBudgetProp(bb, t)
	}
	return bb
}

func (p *parser) parseBudgetProp(bb *ast.BudgetBlock, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenMaxParallelBranches:
		p.expect(TokenColon)
		bb.MaxParallelBranches = p.expectInt()
	case TokenMaxDuration:
		p.expect(TokenColon)
		bb.MaxDuration = p.expectString()
	case TokenMaxCostUSD:
		p.expect(TokenColon)
		bb.MaxCostUSD = p.expectNumber()
	case TokenMaxTokens:
		p.expect(TokenColon)
		bb.MaxTokens = p.expectInt()
	case TokenMaxIterations:
		p.expect(TokenColon)
		bb.MaxIterations = p.expectInt()
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown budget property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

func (p *parser) parseCompactionBlock() *ast.CompactionBlock {
	start := p.next() // consume "compaction"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	cb := &ast.CompactionBlock{Span: ast.Span{Start: p.pos(start)}}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseCompactionProp(cb, t)
	}
	return cb
}

// parseMemoryBlock parses a `memory:` sub-block on an agent or
// judge node. All fields are optional; IR compile applies defaults.
func (p *parser) parseMemoryBlock() *ast.MemoryBlock {
	start := p.next() // consume "memory"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	mb := &ast.MemoryBlock{Span: ast.Span{Start: p.pos(start)}}

	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		p.parseMemoryProp(mb, t)
	}
	return mb
}

func (p *parser) parseCompactionProp(cb *ast.CompactionBlock, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenThreshold:
		p.expect(TokenColon)
		v := p.expectNumber()
		cb.Threshold = &v
	case TokenPreserveRecent:
		p.expect(TokenColon)
		v := p.expectInt()
		cb.PreserveRecent = &v
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown compaction property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

func (p *parser) parseMemoryProp(mb *ast.MemoryBlock, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenEnabled:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			mb.Enabled = v
		}
	case TokenScope:
		p.expect(TokenColon)
		v := p.expectString()
		mb.Scope = &v
	case TokenAutoload:
		p.expect(TokenColon)
		mb.Autoload = p.parseStringList()
	case TokenRead:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			mb.Read = v
		}
	case TokenWrite:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			mb.Write = v
		}
	case TokenPreCompactInject:
		p.expect(TokenColon)
		if v := p.parseBool(); v != nil {
			mb.PreCompactInject = v
		}
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown memory property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

// ---- edge ----

func (p *parser) parseEdge() *ast.Edge {
	fromT := p.next()
	from := tokenAsIdent(fromT)
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
	to := tokenAsIdent(toT)
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

	// Optional clauses: when, as, with (in any order before newline).
	// Reject duplicates — `... when foo when not bar` used to accept
	// the line with the second clause silently overwriting the first.
	// Track each by token kind so the error message points the operator
	// at the right culprit.
	var sawWhen, sawAs, sawWith bool
	for {
		t := p.peek()
		switch t.Type {
		case TokenWhen:
			if sawWhen {
				p.addError(DiagDuplicateEdgeClause, t, "duplicate 'when' clause on edge")
			}
			parsed := p.parseWhenClause()
			if !sawWhen {
				edge.When = parsed
			}
			sawWhen = true
		case TokenAs:
			if sawAs {
				p.addError(DiagDuplicateEdgeClause, t, "duplicate 'as' clause on edge")
			}
			parsed := p.parseLoopClause()
			if !sawAs {
				edge.Loop = parsed
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
	switch nt := p.peek(); nt.Type {
	case TokenInt:
		lc.MaxIterations = p.expectInt()
	case TokenString:
		lc.MaxIterationsExpr = p.expectString()
	default:
		p.addError(DiagExpectedToken, nt, "expected integer or template string for loop cap, got "+nt.Type.String())
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
	case "low", "medium", "high", "xhigh", "max":
		return value
	default:
		p.addError(DiagInvalidValue, t, "expected reasoning effort (low, medium, high, xhigh, max) or a quoted env-substituted string, got '"+t.Value+"'")
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
	default:
		p.addError(DiagInvalidValue, t, "expected session mode (fresh, inherit, inherit_if_available, fork, artifacts_only), got '"+t.Value+"'")
		return ast.SessionFresh
	}
}

func (p *parser) parseIdentList() []string {
	p.expect(TokenLBrack)
	var names []string
	t := p.next()
	id := tokenAsIdent(t)
	if id != "" {
		names = append(names, id)
	}
	for p.peek().Type == TokenComma {
		p.next() // consume ,
		t = p.next()
		id = tokenAsIdent(t)
		if id != "" {
			names = append(names, id)
		}
	}
	p.expect(TokenRBrack)
	return names
}

func (p *parser) parseStringList() []string {
	p.expect(TokenLBrack)
	var vals []string
	if p.peek().Type == TokenRBrack {
		p.next()
		return vals
	}
	vals = append(vals, p.expectString())
	for p.peek().Type == TokenComma {
		p.next()
		vals = append(vals, p.expectString())
	}
	p.expect(TokenRBrack)
	return vals
}

// parseToolList parses a bracketed list of tool references that may contain
// dotted qualified names (e.g. [git_diff, mcp.claude_code.delegate]).
func (p *parser) parseToolList() []string {
	p.expect(TokenLBrack)
	var names []string
	name := p.parseToolRef()
	if name != "" {
		names = append(names, name)
	}
	for p.peek().Type == TokenComma {
		p.next() // consume ,
		name = p.parseToolRef()
		if name != "" {
			names = append(names, name)
		}
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

func (p *parser) expectString() string {
	t := p.next()
	if t.Type == TokenString {
		return t.Value
	}
	p.addError(DiagExpectedToken, t, "expected string literal, got "+t.Type.String())
	return t.Value
}

func (p *parser) expectIdent() string {
	t := p.next()
	id := tokenAsIdent(t)
	if id != "" {
		return id
	}
	p.addError(DiagExpectedToken, t, "expected identifier, got "+t.Type.String())
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
	p.addError(DiagExpectedToken, t, "expected integer, got "+t.Type.String())
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
		p.addError(DiagExpectedToken, t, "expected number, got "+t.Type.String())
		return 0
	}
}

func (p *parser) skipToNewline() {
	for {
		t := p.peek()
		if t.Type == TokenNewline || t.Type == TokenEOF || t.Type == TokenDedent {
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

func isKeywordToken(tt TokenType) bool {
	switch tt {
	case TokenVars, TokenPresets, TokenMCPServer, TokenPrompt, TokenSchema, TokenAgent, TokenJudge,
		TokenRouter, TokenHuman, TokenTool, TokenCompute, TokenWorkflow,
		TokenJoin,
		TokenEntry, TokenMCP, TokenBudget, TokenTransport, TokenServers,
		TokenDisable, TokenAutoloadProject, TokenModel, TokenInput, TokenOutput,
		TokenPublish, TokenSystem, TokenUser, TokenSession, TokenTools, TokenToolPolicy,
		TokenCapabilities, TokenToolMaxSteps, TokenReasoningEffort, TokenMode, TokenStrategy, TokenRequire,
		TokenInstructions, TokenCommand, TokenScript, TokenLanguage, TokenArgs, TokenURL,
		TokenAuth, TokenReadonly,
		TokenDefaultBackend,
		TokenInteraction, TokenInteractionPrompt, TokenInteractionModel,
		TokenBackend, TokenProvider, TokenAwait, TokenWhen, TokenNot, TokenAs,
		TokenWith, TokenEnum, TokenFresh, TokenInherit, TokenArtifactsOnly,
		TokenFork,
		TokenFanOutAll, TokenCondition, TokenRoundRobin, TokenLLM, TokenMulti,
		TokenWaitAll, TokenBestEffort,
		TokenTrue, TokenFalse,
		TokenTypeString, TokenTypeBool, TokenTypeInt, TokenTypeFloat,
		TokenTypeJSON, TokenTypeStringArray,
		TokenMaxParallelBranches, TokenMaxDuration, TokenMaxCostUSD,
		TokenMaxTokens, TokenMaxIterations,
		TokenCompaction, TokenThreshold, TokenPreserveRecent,
		TokenMemory, TokenEnabled, TokenScope, TokenAutoload, TokenRead, TokenWrite, TokenPreCompactInject,
		TokenWorktree,
		TokenSandbox,
		TokenCursor, TokenCursors, TokenValues, TokenBands,
		TokenAttachments, TokenTypeFile, TokenTypeImage,
		TokenDone, TokenFail:
		return true
	}
	return false
}
