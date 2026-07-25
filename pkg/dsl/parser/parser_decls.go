package parser

import (
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

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

	// Optional enum constraint between the type and the default, same
	// syntax as a schema field's: `mode: string [enum: "a", "b"] = "a"`.
	var enumVals []string
	if p.peek().Type == TokenLBrack {
		enumVals = p.parseEnumConstraint()
	}

	var def *ast.Literal
	if p.peek().Type == TokenEquals {
		p.next() // consume =
		def = p.parseLiteral()
	}
	p.skipNewlines()

	return &ast.VarField{
		Name:       name,
		Type:       te,
		EnumValues: enumVals,
		Default:    def,
		Span:       ast.Span{Start: p.pos(nameT), End: p.pos(nameT)},
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

// parseSecretsBlock parses a top-level `secrets:` block. Mirrors
// parseVarsBlock's INDENT/DEDENT loop; each field is a SecretField.
func (p *parser) parseSecretsBlock() *ast.SecretsBlock {
	start := p.next() // consume "secrets"
	p.expect(TokenColon)
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
	}

	sb := &ast.SecretsBlock{Span: ast.Span{Start: p.pos(start)}}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			break
		}
		sf := p.parseSecretField()
		if sf != nil {
			sb.Fields = append(sb.Fields, sf)
		}
	}
	if len(sb.Fields) > 0 {
		sb.Span.End = sb.Fields[len(sb.Fields)-1].Span.End
	} else {
		sb.Span.End = sb.Span.Start
	}
	return sb
}

// parseSecretField parses one secret declaration. Short form
// `name: "value"`; block form with optional `value`, `as`,
// `mount_path`, `env`, `hosts`, `description` sub-properties.
// Mirrors parseAttachmentField.
func (p *parser) parseSecretField() *ast.SecretField {
	nameT := p.next()
	if nameT.Type != TokenIdent && !isKeywordToken(nameT.Type) {
		p.addError(DiagExpectedToken, nameT, "expected secret name, got "+nameT.Type.String())
		p.skipToNewline()
		return nil
	}
	p.expect(TokenColon)

	sf := &ast.SecretField{
		Name: nameT.Value,
		Span: ast.Span{Start: p.pos(nameT), End: p.pos(nameT)},
	}
	// Short form: a quoted value on the same line.
	if p.peek().Type == TokenString {
		sf.Value = p.expectString()
	}
	p.skipNewlines()

	// Optional indented sub-block (value / hosts / description).
	if p.peek().Type != TokenIndent {
		return sf
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
		if t.Type != TokenIdent && !isKeywordToken(t.Type) {
			p.addError(DiagUnexpectedToken, t, "unexpected token in secret block: "+t.Value)
			p.next()
			p.skipToNewline()
			continue
		}
		propName := t.Value
		p.next()
		p.expect(TokenColon)
		switch propName {
		case "value":
			sf.Value = p.expectString()
		case "as":
			sf.As = p.expectStringOrIdent()
		case "mount_path":
			sf.MountPath = p.expectString()
		case "env":
			sf.Env = p.expectStringOrIdent()
		case "optional":
			if v := p.parseBool(); v != nil {
				sf.Optional = *v
			}
		case "hosts":
			sf.Hosts = p.parseStringList()
		case "description":
			sf.Description = p.expectString()
		default:
			p.addError(DiagUnknownProperty, t, "unknown secret property '"+propName+"'")
			p.skipToNewline()
		}
		p.skipNewlines()
	}
	return sf
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
	start, name, ok := p.parseDeclHeader("prompt")
	if !ok {
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
	start, name, ok := p.parseDeclHeader("schema")
	if !ok {
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
	} else {
		// Don't silently drop bare identifiers — enum values must be quoted.
		p.addError(DiagInvalidValue, t, "enum values must be quoted strings, got "+t.Type.String())
	}
	for p.peek().Type == TokenComma {
		p.next() // consume ,
		t = p.next()
		if t.Type == TokenString {
			vals = append(vals, t.Value)
		} else {
			p.addError(DiagInvalidValue, t, "enum values must be quoted strings, got "+t.Type.String())
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
	start, name, ok := p.parseDeclHeader("cursor")
	if !ok {
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

// parseSupervisorDecl parses a top-level `supervisor <name>:`
// declaration: a concurrent node-watcher (see docs/supervisors.md).
// Scalar fields plus a `watches:` ident list. Monitors are not declared
// here — the supervisor bot registers them at runtime — keeping the
// grammar small.
func (p *parser) parseSupervisorDecl() *ast.SupervisorDecl {
	start, name, ok := p.parseDeclHeader("supervisor")
	if !ok {
		return nil
	}
	sd := &ast.SupervisorDecl{
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
			p.addError(DiagUnexpectedToken, t, "unexpected token in supervisor block: "+t.Value)
			p.next()
			p.skipToNewline()
			continue
		}
		p.next() // consume property keyword
		switch propName {
		case "watches":
			p.expect(TokenColon)
			sd.Watches = p.parseIdentList()
			p.skipNewlines()
		case "model":
			p.expect(TokenColon)
			sd.Model = p.expectString()
			p.skipNewlines()
		case "system":
			p.expect(TokenColon)
			sd.System = p.expectIdent()
			p.skipNewlines()
		case "cooldown":
			p.expect(TokenColon)
			sd.Cooldown = p.expectString()
			p.skipNewlines()
		case "max_evals":
			p.expect(TokenColon)
			sd.MaxEvals = p.expectInt()
			p.skipNewlines()
		default:
			p.addError(DiagUnknownProperty, t, "unknown supervisor property '"+propName+"'")
			p.skipToNewline()
		}
	}
	return sd
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
