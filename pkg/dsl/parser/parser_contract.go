package parser

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// contractMembers reads an already-open body, one named member per line.
// Names stay declarations in source order; the compiler, not a map
// assignment, diagnoses a duplicate port or criterion.
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

// contractProperties reads the properties of host: a name listed by the
// registry is read by property, any other draws E012 with the remedy.
func (p *parser) contractProperties(host string, span *ast.Span, property func(Token) bool) {
	seen := map[string]bool{}
	p.contractMembers(span, func(t Token) {
		if seen[t.Value] {
			p.addErrorHint(DiagDuplicateBlock, t, "duplicate '"+t.Value+"' in "+host, "Each property appears once per block: keep one and delete the other.")
		}
		seen[t.Value] = true
		if !property(t) {
			p.unknownProperty(host, t, t.Value)
			p.skipUnknownProperty()
		}
	})
}

// valueEnds reports whether t is what may follow a scalar value: the end of
// its line, or the end of its block or file.
func valueEnds(t Token) bool {
	return lineEnds(t) || t.Type == TokenDedent || t.Type == TokenEOF
}

// endOfValue holds a scalar property to its line: what follows the value
// must close the line. A second value, or the tail of a number the lexer
// read in two pieces (`1e3` is the number 1 and the identifier e3), is
// refused here, at the tail, and the caller stores nothing — never a
// truncated value read on as the next property. Reports whether the line
// was clean.
func (p *parser) endOfValue(what string) bool {
	t := p.peek()
	if valueEnds(t) || t.Type == TokenIndent {
		return true
	}
	p.addError(DiagExpectedToken, t, what+" takes one value on its line, got '"+t.Value+"'")
	p.skipToNewline()
	return false
}

func (p *parser) contractString(name string) string {
	p.expect(TokenColon)
	v := p.expectString()
	if !p.endOfValue(name) {
		return ""
	}
	return v
}

// contractInt reads an integer property; ok is false when the value or
// its line is not what the grammar wants, and nothing is to be stored.
func (p *parser) contractInt(name string) (int, bool) {
	p.expect(TokenColon)
	t, ok := p.expect(TokenInt)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(t.Value)
	if err != nil {
		p.addError(DiagExpectedToken, t, name+" takes an integer, got '"+t.Value+"'")
		return 0, false
	}
	if !p.endOfValue(name) {
		return 0, false
	}
	return n, true
}

func (p *parser) contractBoolProp(name string) (bool, bool) {
	p.expect(TokenColon)
	v, ok := p.contractBool()
	if !ok || !p.endOfValue(name) {
		return false, false
	}
	return v, true
}

// contractIdent reads an identifier value, dotted when the property is a
// reference (`from: node.field`, `port: input.name`, a plugin's `kind`).
func (p *parser) contractIdent(name string, dotted bool) string {
	p.expect(TokenColon)
	v := p.expectIdent()
	if dotted && v != "" {
		v = p.continueDottedRef(v)
	}
	if !p.endOfValue(name) {
		return ""
	}
	return v
}

func (p *parser) contractJSONProp() json.RawMessage {
	p.expect(TokenColon)
	return p.contractJSON()
}

// parseContractDecl reads `contract <name>:` and its body.
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
			c.DisplayName = p.contractString(t.Value)
		case "responsibility":
			c.Responsibility = p.contractString(t.Value)
		case "version":
			if v, ok := p.contractInt(t.Value); ok {
				c.Version = &v
			}
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

// parseContractPorts reads an `inputs:` or `outputs:` block: `name: type`
// entries, each with optional indented properties. A type is a builtin or a
// declared schema's name, with `[]` suffixes — a name, not a reference.
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
			port.Type = p.expectIdent()
		}
		for p.peek().Type == TokenLBrack {
			p.next()
			p.expect(TokenRBrack)
			port.Type += "[]"
		}
		if !p.endOfValue("a port's type") {
			port.Type = ""
		}
		p.skipNewlines()
		if p.peek().Type == TokenIndent {
			p.next()
			p.contractProperties("contract.port", &port.Span, func(prop Token) bool {
				switch prop.Value {
				case "description":
					port.Description = p.contractString(prop.Value)
				case "required":
					if v, ok := p.contractBoolProp(prop.Value); ok {
						port.Required = &v
					}
				case "nullable":
					if v, ok := p.contractBoolProp(prop.Value); ok {
						port.Nullable = v
					}
				case "default":
					port.Default = p.contractJSONProp()
				case "min_items":
					if v, ok := p.contractInt(prop.Value); ok {
						port.MinItems = &v
					}
				case "max_items":
					if v, ok := p.contractInt(prop.Value); ok {
						port.MaxItems = &v
					}
				case "from":
					port.From = p.contractIdent(prop.Value, true)
				case "file":
					port.FileSpec = p.parsePortFile(prop)
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
			f.MediaType = p.contractString(prop.Value)
		case "min_bytes":
			if v, ok := p.contractInt(prop.Value); ok {
				f.MinBytes = int64(v)
			}
		case "schema":
			f.Schema = p.contractIdent(prop.Value, false)
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
			c.Kind = p.contractIdent(prop.Value, true)
		case "port":
			c.Port = p.contractIdent(prop.Value, true)
		case "params":
			c.Params = p.contractJSONProp()
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
			e.Description = p.contractString(prop.Value)
		case "paid":
			if v, ok := p.contractBoolProp(prop.Value); ok {
				e.Paid = v
			}
		default:
			return false
		}
		return true
	})
	return e
}

// contractBool reads `true` or `false`; ok is false on anything else.
func (p *parser) contractBool() (bool, bool) {
	t := p.next()
	if t.Type != TokenTrue && t.Type != TokenFalse {
		p.addError(DiagExpectedToken, t, "expected true or false")
		return false, false
	}
	return t.Type == TokenTrue, true
}

// jsonValueRule is what a JSON value is in the text, said in every refusal.
const jsonValueRule = "a JSON value is one value on one line — \"text\", 12, true, null, [...] or {key: value}; a .bot has no signed number and no exponent: write a positive decimal, or a string"

// contractJSON reads a default value or criterion parameters as structured
// data, in the document's ordinary quoting, and holds the value to its
// line. A number keeps its precision (json.Number) and `null` is stored
// explicitly, never read as an absent default. The text writes only what
// its lexer reads — no signed number, no exponent — so a value read here is
// writable by construction; the same rule is held on a value that came
// from the transport by WritableJSONValue.
func (p *parser) contractJSON() json.RawMessage {
	start := p.peek()
	value, ok := p.contractJSONValue()
	if !ok {
		return nil
	}
	if t := p.peek(); !valueEnds(t) {
		p.addError(DiagExpectedToken, t, jsonValueRule+" (got '"+t.Value+"' after the value)")
		p.skipToNewline()
		return nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		p.addError(DiagExpectedToken, start, "invalid JSON value: "+err.Error())
		return nil
	}
	return b
}

// contractJSONValue reads one value. A container left open at the end of
// its line stops there, at the line — the tokens after it are the next
// lines' own, never consumed as a key or a member.
func (p *parser) contractJSONValue() (any, bool) {
	if t := p.peek(); valueEnds(t) {
		p.addError(DiagExpectedToken, t, "expected a JSON value: "+jsonValueRule)
		return nil, false
	}
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
			if n := p.peek(); valueEnds(n) {
				p.addError(DiagExpectedToken, n, "expected a JSON value or ']': "+jsonValueRule)
				return nil, false
			}
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
			if keyTok.Type != TokenString && keyTok.Type != TokenIdent && !isKeywordToken(keyTok.Type) {
				p.addError(DiagExpectedToken, keyTok, "expected a JSON object key or '}': "+jsonValueRule)
				return nil, false
			}
			key := p.expectStringOrIdent()
			if _, exists := values[key]; exists {
				p.addErrorHint(DiagDuplicateBlock, keyTok, "duplicate JSON key '"+key+"'", "A JSON object names each key once: keep one and delete the other.")
			}
			if _, ok := p.expect(TokenColon); !ok {
				return nil, false
			}
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
		p.addError(DiagExpectedToken, t, fmt.Sprintf("expected a JSON value, got %q: %s", t.Value, jsonValueRule))
		return nil, false
	}
}
