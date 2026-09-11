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
			nb.Preset = p.expectStringOrIdent()
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
		out[key] = p.expectStringOrIdent()
		p.skipNewlines()
	}
	return out
}

// parseStringOrIdentList parses a `[a, b, c]` or `["a", "b"]` form.
// Unlike the existing parseStringList helper which requires every
// element to be a quoted string, this one also accepts bare idents
// — useful for sandbox.network.rules where authors mix quoted globs
// like "!**.evil.site" and bare hostnames like github.com.
func (p *parser) parseStringOrIdentList() []string {
	if p.peek().Type == TokenNewline {
		return p.parseDashList(func() (string, bool) {
			v := p.expectStringOrIdent()
			return v, v != ""
		})
	}
	if _, ok := p.expect(TokenLBrack); !ok {
		return nil
	}
	var out []string
	for {
		t := p.peek()
		if t.Type == TokenRBrack || t.Type == TokenEOF {
			if t.Type == TokenRBrack {
				p.next()
			}
			return out
		}
		out = append(out, p.expectStringOrIdent())
		cm := p.peek()
		if cm.Type == TokenComma {
			p.next()
		}
	}
}

// expectStringOrIdent accepts either a quoted string literal or a
// bare ident (lifted as a string). Used in heterogeneous list/map
// forms where users mix quoted globs and bare hostnames.
func (p *parser) expectStringOrIdent() string {
	t := p.peek()
	if t.Type == TokenString {
		return p.expectString()
	}
	if t.Type == TokenIdent || isKeywordToken(t.Type) {
		// A bare hostname is dotted (`github.com`): read the whole of it.
		return p.continueDottedRef(p.expectIdent())
	}
	p.addError(DiagExpectedToken, t, "expected string or identifier")
	p.next()
	return ""
}
