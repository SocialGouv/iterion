package parser

import "github.com/SocialGouv/iterion/pkg/dsl/ast"

// business_stores stays an identifier so an existing node with this name
// retains its grammar. An entry's quoted ID can name an external identity.
func (p *parser) parseBusinessStoresBlock() []*ast.BusinessStoreDecl {
	defer p.enterBlock("business_stores")()
	out := make([]*ast.BusinessStoreDecl, 0)
	switch p.parseBlockBody() {
	case headerFailed:
		return nil
	case headerEmpty:
		return out
	}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			return out
		}
		id, ok := p.expectStringOrIdentOK()
		if !ok {
			p.skipToNewline()
			continue
		}
		entry := &ast.BusinessStoreDecl{ID: id, Span: ast.Span{Start: p.pos(t), End: p.pos(t)}}
		p.expect(TokenColon)
		p.skipNewlines()
		if p.peek().Type == TokenIndent {
			p.next()
			p.parseBusinessStoreBody(entry)
		}
		out = append(out, entry)
	}
}

func (p *parser) parseBusinessStoreBody(entry *ast.BusinessStoreDecl) {
	defer p.enterBlock("business_store")()
	for {
		p.skipNewlines()
		t := p.peek()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			if t.Type == TokenDedent {
				p.next()
			}
			return
		}
		p.next()
		p.expect(TokenColon)
		switch t.Value {
		case "kind":
			entry.Kind = p.expectStringOrIdentLine()
		case "namespace":
			entry.Namespace = p.expectStringOrIdentLine()
		case "required":
			entry.Required = p.parseBool()
		default:
			p.unknownProperty("business_store", t, t.Value)
			p.skipUnknownProperty()
		}
		entry.Span.End = p.pos(t)
	}
}
