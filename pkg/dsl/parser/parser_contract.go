package parser

import (
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// contractMembers reads an already-open body. Names remain declarations in
// source order; the compiler, not a map assignment, diagnoses duplicate ports,
// instances, bindings and exports.
func (p *parser) contractMembers(span *ast.Span, member func(Token)) {
	for {
		p.skipNewlines()
		t := p.next()
		if t.Type == TokenDedent || t.Type == TokenEOF {
			span.End = p.pos(t)
			return
		}
		if t.Type != TokenIdent && !isKeywordToken(t.Type) {
			p.addError(DiagExpectedToken, t, "expected a named contract member")
			p.skipToNewline()
			continue
		}
		member(t)
	}
}

func (p *parser) contractProperties(host string, span *ast.Span, property func(Token) bool) {
	seen := map[string]bool{}
	p.contractMembers(span, func(t Token) {
		if seen[t.Value] {
			p.addError(DiagDuplicateBlock, t, "duplicate '"+t.Value+"' in "+host)
		}
		seen[t.Value] = true
		if !property(t) {
			p.unknownProperty(host, t, t.Value)
			p.skipUnknownProperty()
		}
	})
}

func (p *parser) parseContractDecl() *ast.ContractDecl {
	start, name, state := p.parseDeclHeaderOrEmpty("contract")
	if state == headerFailed {
		return nil
	}
	c := &ast.ContractDecl{Name: name, Span: ast.Span{Start: p.pos(start)}}
	if state == headerEmpty {
		return c
	}
	p.contractProperties("contract", &c.Span, func(t Token) bool {
		switch t.Value {
		case "display_name":
			p.expect(TokenColon)
			c.DisplayName = p.expectString()
		case "responsibility":
			p.expect(TokenColon)
			c.Responsibility = p.expectString()
		case "version":
			p.expect(TokenColon)
			c.Version = p.expectInt()
		case "inputs":
			c.Inputs = p.parseContractPorts()
		case "outputs":
			c.Outputs = p.parseContractPorts()
		case "criteria":
			if p.parseBlockBody() == headerBody {
				span := ast.Span{Start: p.pos(t)}
				p.contractMembers(&span, func(entry Token) {
					c.Criteria = append(c.Criteria, p.parseContractCriterion(entry))
				})
			}
		case "effects":
			if p.parseBlockBody() == headerBody {
				span := ast.Span{Start: p.pos(t)}
				p.contractMembers(&span, func(entry Token) {
					c.Effects = append(c.Effects, p.parsePublicEffect(entry))
				})
			}
		default:
			return false
		}
		return true
	})
	return c
}

func (p *parser) parseContractPorts() []*ast.PortDecl {
	if p.parseBlockBody() != headerBody {
		return nil
	}
	var ports []*ast.PortDecl
	var span ast.Span
	p.contractMembers(&span, func(t Token) {
		p.expect(TokenColon)
		port := &ast.PortDecl{Name: t.Value, Span: ast.Span{Start: p.pos(t)}}
		if p.peek().Type == TokenTypeStringArray {
			port.Type = p.next().Value
		} else {
			port.Type = p.continueDottedRef(p.expectIdent())
		}
		for p.peek().Type == TokenLBrack {
			p.next()
			p.expect(TokenRBrack)
			port.Type += "[]"
		}
		p.skipNewlines()
		if p.peek().Type == TokenIndent {
			p.next()
			p.contractProperties("contract.port", &port.Span, func(prop Token) bool {
				switch prop.Value {
				case "description":
					p.expect(TokenColon)
					port.Description = p.expectString()
				case "required":
					p.expect(TokenColon)
					v := p.contractBool()
					port.Required = &v
				case "nullable":
					p.expect(TokenColon)
					port.Nullable = p.contractBool()
				case "default":
					p.expect(TokenColon)
					port.Default = p.contractJSON()
				case "min_items":
					p.expect(TokenColon)
					v := p.expectInt()
					port.MinItems = &v
				case "max_items":
					p.expect(TokenColon)
					v := p.expectInt()
					port.MaxItems = &v
				case "file":
					port.File = p.parsePortFile(prop)
				default:
					return false
				}
				return true
			})
		}
		ports = append(ports, port)
	})
	return ports
}

func (p *parser) parsePortFile(t Token) *ast.PortFileDecl {
	f := &ast.PortFileDecl{Span: ast.Span{Start: p.pos(t)}}
	if p.parseBlockBody() != headerBody {
		return f
	}
	p.contractProperties("contract.file", &f.Span, func(prop Token) bool {
		switch prop.Value {
		case "media_type":
			p.expect(TokenColon)
			f.MediaType = p.expectString()
		case "min_bytes":
			p.expect(TokenColon)
			f.MinBytes = int64(p.expectInt())
		case "schema":
			p.expect(TokenColon)
			f.Schema = p.expectIdent()
		default:
			return false
		}
		return true
	})
	return f
}

func (p *parser) parseContractCriterion(t Token) *ast.CriterionDecl {
	c := &ast.CriterionDecl{Name: t.Value, Span: ast.Span{Start: p.pos(t)}}
	if p.parseBlockBody() != headerBody {
		return c
	}
	p.contractProperties("contract.criterion", &c.Span, func(prop Token) bool {
		switch prop.Value {
		case "kind":
			p.expect(TokenColon)
			c.Kind = p.expectIdent()
		case "port":
			p.expect(TokenColon)
			c.Port = p.continueDottedRef(p.expectIdent())
		case "params":
			p.expect(TokenColon)
			c.Params = p.contractJSON()
		default:
			return false
		}
		return true
	})
	return c
}

func (p *parser) parsePublicEffect(t Token) *ast.PublicEffect {
	e := &ast.PublicEffect{Name: t.Value, Span: ast.Span{Start: p.pos(t)}}
	if p.parseBlockBody() != headerBody {
		return e
	}
	p.contractProperties("contract.effect", &e.Span, func(prop Token) bool {
		switch prop.Value {
		case "description":
			p.expect(TokenColon)
			e.Description = p.expectString()
		case "paid":
			p.expect(TokenColon)
			e.Paid = p.contractBool()
		default:
			return false
		}
		return true
	})
	return e
}

func (p *parser) parsePortGraph() *ast.PortGraphDecl {
	t := p.next()
	g := &ast.PortGraphDecl{Span: ast.Span{Start: p.pos(t)}}
	if p.parseBlockBody() != headerBody {
		return g
	}
	p.contractProperties("graph", &g.Span, func(prop Token) bool {
		switch prop.Value {
		case "nodes":
			if p.parseBlockBody() == headerBody {
				var span ast.Span
				p.contractMembers(&span, func(entry Token) {
					g.Nodes = append(g.Nodes, p.parsePortNode(entry))
				})
			}
		case "bindings":
			if p.parseBlockBody() == headerBody {
				var span ast.Span
				p.contractMembers(&span, func(entry Token) {
					b := &ast.PortBindingDecl{Span: ast.Span{Start: p.pos(entry)}}
					b.From = p.continueDottedRef(entry.Value)
					p.expect(TokenArrow)
					b.To = p.continueDottedRef(p.expectIdent())
					b.Span.End = p.pos(p.peek())
					g.Bindings = append(g.Bindings, b)
				})
			}
		case "exports":
			if p.parseBlockBody() == headerBody {
				var span ast.Span
				p.contractMembers(&span, func(entry Token) {
					p.expect(TokenColon)
					e := &ast.PortExportDecl{Name: entry.Value, Span: ast.Span{Start: p.pos(entry)}}
					e.From = p.continueDottedRef(p.expectIdent())
					e.Span.End = p.pos(p.peek())
					g.Exports = append(g.Exports, e)
				})
			}
		case "products":
			p.expect(TokenColon)
			g.Products = p.parseStringList()
		default:
			return false
		}
		return true
	})
	return g
}

func (p *parser) parsePortNode(t Token) *ast.PortNodeDecl {
	n := &ast.PortNodeDecl{Name: t.Value, Span: ast.Span{Start: p.pos(t)}}
	if p.parseBlockBody() != headerBody {
		return n
	}
	p.contractProperties("graph.node", &n.Span, func(prop Token) bool {
		switch prop.Value {
		case "implementation":
			p.expect(TokenColon)
			n.Implementation = p.continueDottedRef(p.expectIdent())
		case "contract":
			p.expect(TokenColon)
			n.Contract = p.expectIdent()
		case "policy":
			p.expect(TokenColon)
			n.Policy = p.expectIdent()
		default:
			return false
		}
		return true
	})
	return n
}

func (p *parser) parsePortPolicyDecl() *ast.PortPolicyDecl {
	start, name, state := p.parseDeclHeaderOrEmpty("port_policy")
	if state == headerFailed {
		return nil
	}
	policy := &ast.PortPolicyDecl{Name: name, Span: ast.Span{Start: p.pos(start)}}
	if state == headerEmpty {
		return policy
	}
	p.contractProperties("port_policy", &policy.Span, func(t Token) bool {
		switch t.Value {
		case "max_map_items":
			p.expect(TokenColon)
			policy.MaxMapItems = p.expectInt()
		case "effects":
			if p.parseBlockBody() == headerBody {
				var span ast.Span
				p.contractMembers(&span, func(entry Token) {
					e := &ast.EffectPolicyDecl{Name: entry.Value, Span: ast.Span{Start: p.pos(entry)}}
					if p.parseBlockBody() == headerBody {
						p.contractProperties("port_policy.effect", &e.Span, func(prop Token) bool {
							switch prop.Value {
							case "resource":
								p.expect(TokenColon)
								e.Resource = p.expectIdent()
							case "recovery":
								p.expect(TokenColon)
								e.Recovery = p.expectIdent()
							case "verifier":
								p.expect(TokenColon)
								e.Verifier = p.expectString()
							default:
								return false
							}
							return true
						})
					}
					policy.Effects = append(policy.Effects, e)
				})
			}
		default:
			return false
		}
		return true
	})
	return policy
}

func (p *parser) contractBool() bool {
	t := p.next()
	if t.Type != TokenTrue && t.Type != TokenFalse {
		p.addError(DiagExpectedToken, t, "expected true or false")
	}
	return t.Type == TokenTrue
}

// Default values and criterion parameters are structured data, using the
// document's ordinary string quoting. Numbers retain precision and null is
// stored explicitly, rather than becoming an absent default.
func (p *parser) contractJSON() json.RawMessage {
	start := p.peek()
	value, ok := p.contractJSONValue()
	if !ok {
		return nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		p.addError(DiagExpectedToken, start, "invalid JSON value: "+err.Error())
		return nil
	}
	return b
}

func (p *parser) contractJSONValue() (any, bool) {
	t := p.next()
	switch t.Type {
	case TokenString:
		return t.Value, true
	case TokenInt, TokenFloat:
		return json.Number(t.Value), true
	case TokenTrue:
		return true, true
	case TokenFalse:
		return false, true
	case TokenLBrack:
		values := make([]any, 0)
		for p.peek().Type != TokenRBrack {
			v, ok := p.contractJSONValue()
			if !ok {
				return nil, false
			}
			values = append(values, v)
			if p.peek().Type != TokenComma {
				break
			}
			p.next()
		}
		_, ok := p.expect(TokenRBrack)
		return values, ok
	case TokenLBrace:
		values := map[string]any{}
		for p.peek().Type != TokenRBrace {
			keyTok := p.peek()
			key := p.expectStringOrIdent()
			if _, exists := values[key]; exists {
				p.addError(DiagDuplicateBlock, keyTok, "duplicate JSON key '"+key+"'")
			}
			p.expect(TokenColon)
			v, ok := p.contractJSONValue()
			if !ok {
				return nil, false
			}
			values[key] = v
			if p.peek().Type != TokenComma {
				break
			}
			p.next()
		}
		_, ok := p.expect(TokenRBrace)
		return values, ok
	default:
		if t.Type == TokenIdent && t.Value == "null" {
			return nil, true
		}
		p.addError(DiagExpectedToken, t, fmt.Sprintf("expected a JSON value, got %q", t.Value))
		return nil, false
	}
}
