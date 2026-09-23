package parser

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// Sandbox sub-grammar — `sandbox:` declaration + its `build:` /
// `network:` sub-blocks + shared string-map / string-or-ident helpers.
// Carved out of parser.go to keep that file focused on top-level + node
// grammar. Same package; no external API change.

// parseSandboxBlock handles both forms of the `sandbox:` declaration:
//
//	sandbox: ident                  # short form (none / auto / inline)
//	sandbox:                        # block form
//	  image: "..."
//	  env:
//	    KEY: value
//	  network:
//	    mode: allowlist
//	    rules: [...]
//
// The short form is folded into the block struct with Mode set and
// every other field zero. The block form derives Mode from the
// presence of body fields when not declared explicitly: the parser
// sets Mode="inline" so the IR compiler routes it through the
// driver-spec converter rather than the devcontainer.json reader.
func (p *parser) parseSandboxBlock(host string) *ast.SandboxBlock {
	defer p.enterBlock(host)()
	start := p.next() // consume "sandbox"
	colon, _ := p.expect(TokenColon)

	sb := &ast.SandboxBlock{Span: ast.Span{Start: p.pos(start)}}

	// Look ahead: if the next non-newline token starts on the next
	// indented line we're in block form; otherwise it's the short
	// form (a single ident on the same logical line).
	t := p.peek()
	if t.Type == TokenIdent || isKeywordToken(t.Type) {
		// Short form: a single ident immediately follows the colon.
		ident := p.expectIdent()
		sb.Mode = ident
		return sb
	}

	// Block form.
	switch p.blockBodyAfter(colon) {
	case headerFailed:
		return sb // reported; the node still parses
	case headerEmpty:
		sb.Mode = "inline" // the block form with nothing in it
		return sb
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
		p.parseSandboxProp(sb, t)
	}
	if sb.Mode == "" {
		// Block-form without an explicit mode → inline.
		sb.Mode = "inline"
	}
	return sb
}

// parseSandboxProp dispatches one property line inside a `sandbox:`
// block. Property names use the literal token .Value (rather than
// dedicated keyword tokens) to keep the surface small — none of these
// names collide with existing top-level keywords.
func (p *parser) parseSandboxProp(sb *ast.SandboxBlock, propTok Token) {
	if propTok.Type != TokenIdent && !isKeywordToken(propTok.Type) {
		p.addError(DiagUnexpectedToken, propTok, "unexpected token '"+propTok.Value+"' in sandbox block")
		p.next()
		p.skipToNewline()
		return
	}
	name := propTok.Value
	p.next() // consume the property identifier
	colon, _ := p.expect(TokenColon)

	switch name {
	case "mode":
		sb.Mode = p.expectIdent()
	case "image":
		sb.Image = p.expectString()
	case "user":
		sb.User = p.expectString()
	case "workspace_folder":
		sb.WorkspaceFolder = p.expectString()
	case "host_state":
		sb.HostState = p.expectIdent()
	case "post_create":
		sb.PostCreate = p.expectString()
	case "env":
		sb.Env = p.parseStringMapBlock()
	case "mounts":
		sb.Mounts = p.parseStringOrIdentList()
	case "network":
		// The "network" keyword and trailing colon are already
		// consumed by the parseSandboxProp prologue above, so we
		// pass the propTok span directly to keep diagnostics
		// pointing at the right position.
		sb.Network = p.parseSandboxNetworkBody(propTok, colon)
	case "build":
		// V2-6: Dockerfile-based image build. Mutually exclusive with
		// `image:` (enforced at IR compile time, not the parser).
		sb.Build = p.parseSandboxBuildBody(propTok, colon)
	default:
		p.unknownProperty("sandbox", propTok, name)
		p.skipUnknownProperty()
	}
	p.skipNewlines()
}

// parseSandboxBuildBody parses the body of a `build:` sub-block under
// `sandbox:` (V2-6). The opening "build:" tokens have already been
// consumed by parseSandboxProp; we go straight to the indent + body
// loop. Recognised properties: dockerfile (string), context (string),
// args (string map). Unknown properties produce DiagUnknownProperty.
func (p *parser) parseSandboxBuildBody(startTok, colon Token) *ast.SandboxBuildBlock {
	bb := &ast.SandboxBuildBlock{Span: ast.Span{Start: p.pos(startTok)}}
	switch p.blockBodyAfter(colon) {
	case headerFailed:
		return nil
	case headerEmpty:
		return bb
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
			p.addError(DiagUnexpectedToken, t, "unexpected token '"+t.Value+"' in sandbox.build block")
			p.next()
			p.skipToNewline()
			continue
		}
		name := t.Value
		p.next()
		p.expect(TokenColon)
		switch name {
		case "dockerfile":
			bb.Dockerfile = p.expectString()
		case "context":
			bb.Context = p.expectString()
		case "args":
			bb.Args = p.parseStringMapBlock()
		default:
			p.unknownProperty("sandbox.build", t, name)
			p.skipUnknownProperty()
		}
		p.skipNewlines()
	}
	return bb
}

// parseSandboxNetworkBody parses the body of a `network:` sub-block
// under `sandbox:`. The opening "network:" tokens have already been
// consumed by the caller (parseSandboxProp), so we go straight to
// the indent + body loop.
//
// startTok is the original "network" keyword token, used only to
// anchor the Span on the returned struct.
func (p *parser) parseSandboxNetworkBody(startTok, colon Token) *ast.SandboxNetworkBlock {
	nb := &ast.SandboxNetworkBlock{Span: ast.Span{Start: p.pos(startTok)}}
	switch p.blockBodyAfter(colon) {
	case headerFailed:
		return nil
	case headerEmpty:
		return nb
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
			p.addError(DiagUnexpectedToken, t, "unexpected token '"+t.Value+"' in sandbox.network block")
			p.next()
			p.skipToNewline()
			continue
		}
		name := t.Value
		p.next()
		p.expect(TokenColon)
		switch name {
		case "mode":
			nb.Mode = p.expectIdent()
		case "preset":
			// Preset names use kebab-case ("iterion-default") which
			// the lexer tokenises into ident/-/ident, so accept a
			// quoted string here; bare idents work for hyphen-free
			// names.
			nb.Preset = p.expectStringOrIdentLine()
		case "inherit":
			nb.Inherit = p.expectIdent()
		case "rules":
			nb.Rules = p.parseStringOrIdentList()
		default:
			p.unknownProperty("sandbox.network", t, name)
			p.skipUnknownProperty()
		}
		p.skipNewlines()
	}
	return nb
}

// parseStringMapBlock parses an inline-or-block string map. Forms:
//
//	env: { KEY1: "v1", KEY2: "v2" }
//	env:
//	  KEY1: "v1"
//	  KEY2: "v2"
//
// Values may be bare idents (lifted as strings) or quoted strings.
func (p *parser) parseStringMapBlock() map[string]string {
	out := make(map[string]string)
	t := p.peek()
	if t.Type == TokenLBrace {
		p.next() // consume "{"
		for {
			tt := p.peek()
			if tt.Type == TokenRBrace || tt.Type == TokenEOF {
				if tt.Type == TokenRBrace {
					p.next()
				}
				break
			}
			key := p.expectIdent()
			p.expect(TokenColon)
			out[key] = p.expectStringOrIdent()
			cm := p.peek()
			if cm.Type == TokenComma {
				p.next()
			}
		}
		return out
	}
	// Block form
	p.skipNewlines()
	if _, ok := p.expect(TokenIndent); !ok {
		return out
	}
	for {
		p.skipNewlines()
		tt := p.peek()
		if tt.Type == TokenDedent || tt.Type == TokenEOF {
			if tt.Type == TokenDedent {
				p.next()
			}
			break
		}
		key := p.expectIdent()
		p.expect(TokenColon)
		if v, ok := p.expectStringOrIdentOK(); ok {
			out[key] = v
		} else {
			// The refused value and its tail go together: read on, the tail
			// became keys of the map (`KEY1: 123 456` gave a key `456`).
			p.skipToNewline()
		}
		p.skipNewlines()
	}
	return out
}

// parseStringOrIdentList parses a `[a, b, c]` or `["a", "b"]` form.
// Unlike the existing parseStringList helper which requires every
// element to be a quoted string, this one also accepts bare idents
// — useful for sandbox.network.rules where authors mix quoted globs
// like "!**.evil.site" and bare hostnames like github.com.
// It is the one list reader every list goes through (parseBracketList): a
// refused element is reported and left out, the rest of the list read. Its
// own inline loop used to APPEND the refusal as an empty string — an empty
// egress rule beside the diagnostic.
func (p *parser) parseStringOrIdentList() []string {
	return p.parseBracketList(func() (string, bool) {
		t := p.peek()
		v, ok := p.expectStringOrIdentOK()
		if ok && v == "" {
			// An empty string is neither a rule nor a mount: the sandbox
			// refuses it when it starts (netproxy: "empty rule"), long after
			// the author could act. Said here, left out, the rest read.
			p.addErrorHint(DiagExpectedToken, t, "an empty string is not a rule or a mount", "Delete the empty element, or write the pattern.")
			return "", false
		}
		return v, ok
	})
}

// expectStringOrIdent accepts either a quoted string literal or a
// bare ident (lifted as a string). Used in heterogeneous list/map
// forms where users mix quoted globs and bare hostnames.
func (p *parser) expectStringOrIdent() string {
	v, _ := p.expectStringOrIdentOK()
	return v
}

// expectStringOrIdentLine is expectStringOrIdent for a property that owns
// its line: a refused value takes the rest of the line with it, so the tail
// of a malformed value is never read as the next property (`posture: 123
// 456` used to draw "unknown property '456'"). The map's `{…}` form and a
// JSON object's keys read one token at a time on a shared line and keep
// the plain reader.
func (p *parser) expectStringOrIdentLine() string {
	v, ok := p.expectStringOrIdentOK()
	if !ok {
		p.skipToNewline()
	}
	return v
}

// expectStringOrIdentOK is expectStringOrIdent reporting whether it read a
// value: a list reader appends nothing on a refusal, where the empty string
// of the plain form would be an element.
func (p *parser) expectStringOrIdentOK() (string, bool) {
	t := p.peek()
	if t.Type == TokenString {
		return p.expectString(), true
	}
	if t.Type == TokenIdent || isKeywordToken(t.Type) {
		// A bare hostname is dotted (`github.com`): read the whole of it.
		return p.continueDottedRef(p.expectIdent()), true
	}
	p.addError(DiagExpectedToken, t, "expected string or identifier")
	p.next()
	return "", false
}
