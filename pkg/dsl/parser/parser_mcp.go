package parser

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// ---- mcp_server ----
//
// Carved out of parser.go to keep that file's bulk focused on the
// top-level + node-declaration grammar. Same package; no API change.

func (p *parser) parseMCPServerDecl() *ast.MCPServerDecl {
	start := p.next() // consume "mcp_server"
	nameT := p.next()
	name := tokenAsIdent(nameT)
	if name == "" {
		p.addError(DiagExpectedToken, nameT, "expected mcp_server name")
		p.skipToNextTopLevel()
		return nil
	}
	colon, _ := p.expect(TokenColon)
	blank := p.skipNewlinesFrom(colon.Line)
	md := &ast.MCPServerDecl{
		Name: name,
		Span: ast.Span{Start: p.pos(start)},
	}
	if p.bodyIsEmpty(blank) {
		return md
	}
	if _, ok := p.expect(TokenIndent); !ok {
		return nil
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
		p.parseMCPServerProp(md, t)
	}
	return md
}

func (p *parser) parseMCPServerProp(md *ast.MCPServerDecl, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenTransport:
		p.expect(TokenColon)
		md.Transport = p.parseMCPTransport()
	case TokenCommand:
		p.expect(TokenColon)
		md.Command = p.expectString()
	case TokenArgs:
		p.expect(TokenColon)
		md.Args = p.parseStringList()
	case TokenURL:
		p.expect(TokenColon)
		md.URL = p.expectString()
	case TokenAuth:
		md.Auth = p.parseMCPAuthBlock(propTok)
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown mcp_server property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}

// parseMCPAuthBlock parses an `auth:` block under an `mcp_server`. The
// `auth` keyword has already been consumed by the caller. The body uses
// indent-block syntax with one property per line.
//
// Recognised properties (matched by identifier value to avoid polluting
// the global keyword namespace):
//
//	type:       "oauth2"            (string, required)
//	auth_url:   "https://..."       (string, required for oauth2)
//	token_url:  "https://..."       (string, required for oauth2)
//	revoke_url: "https://..."       (string, optional)
//	client_id:  "..."               (string, required for oauth2)
//	scopes:     ["repo", "read:org"] (string list, optional)
func (p *parser) parseMCPAuthBlock(authTok Token) *ast.MCPAuthDecl {
	auth := &ast.MCPAuthDecl{Span: ast.Span{Start: p.pos(authTok)}}
	switch p.parseBlockBody() {
	case headerFailed:
		return nil
	case headerEmpty:
		return auth
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
		if t.Type != TokenIdent {
			p.addError(DiagUnknownProperty, t, "unknown auth property '"+t.Value+"'")
			p.next()
			p.skipToNewline()
			continue
		}
		propTok := p.next()
		p.expect(TokenColon)
		switch propTok.Value {
		case "type":
			auth.Type = p.expectString()
		case "auth_url":
			auth.AuthURL = p.expectString()
		case "token_url":
			auth.TokenURL = p.expectString()
		case "revoke_url":
			auth.RevokeURL = p.expectString()
		case "client_id":
			auth.ClientID = p.expectString()
		case "scopes":
			auth.Scopes = p.parseStringList()
		default:
			p.addError(DiagUnknownProperty, propTok, "unknown auth property '"+propTok.Value+"'")
			p.skipToNewline()
		}
		p.skipNewlines()
	}
	return auth
}

func (p *parser) parseMCPTransport() ast.MCPTransport {
	t := p.next()
	value := tokenAsIdent(t)
	switch value {
	case "stdio":
		return ast.MCPTransportStdio
	case "http":
		return ast.MCPTransportHTTP
	case "sse":
		return ast.MCPTransportSSE
	default:
		p.addError(DiagInvalidValue, t, "expected MCP transport (stdio, http, sse), got '"+t.Value+"'")
		return ast.MCPTransportUnknown
	}
}

func (p *parser) parseMCPConfigBlock() *ast.MCPConfigDecl {
	start := p.next() // consume "mcp"
	cfg := &ast.MCPConfigDecl{Span: ast.Span{Start: p.pos(start)}}
	switch p.parseBlockBody() {
	case headerFailed:
		return nil
	case headerEmpty:
		return cfg
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
		p.parseMCPConfigProp(cfg, t)
	}
	return cfg
}

func (p *parser) parseMCPConfigProp(cfg *ast.MCPConfigDecl, propTok Token) {
	p.next()
	switch propTok.Type {
	case TokenAutoloadProject:
		p.expect(TokenColon)
		cfg.AutoloadProject = p.parseBool()
	case TokenInherit:
		p.expect(TokenColon)
		cfg.Inherit = p.parseBool()
	case TokenServers:
		p.expect(TokenColon)
		cfg.Servers = p.parseIdentList()
	case TokenDisable:
		p.expect(TokenColon)
		cfg.Disable = p.parseIdentList()
	default:
		p.addError(DiagUnknownProperty, propTok, "unknown mcp property '"+propTok.Value+"'")
		p.skipToNewline()
	}
	p.skipNewlines()
}
