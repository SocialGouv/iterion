package parser

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// InlinePromptName is the name of the prompt a text written inline on a
// referencing property becomes: derived from the body alone, so it is the
// same whatever node references it, wherever the node sits (a group member
// included), and whatever the node is renamed to — and two references to
// the same text name the same prompt.
func InlinePromptName(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "_inline_" + hex.EncodeToString(sum[:])[:12]
}

// promptRef reads the value of a prompt-reference property — `system:`,
// `user:`, `instructions:` on the kinds that carry one. A bare name refers
// to a declared prompt, as ever. A string — quoted, raw, or a `|` block
// scalar, whose paragraph breaks and trailing newline it keeps — is the
// prompt's text itself: it becomes an Inline prompt of the file, named
// after its body, and the property refers to it. That is the shape the
// language's own front page taught, and the one every first draft writes.
func (p *parser) promptRef() string {
	if p.peek().Type != TokenString {
		return p.expectIdent()
	}
	t := p.next()
	name := InlinePromptName(t.Value)
	if p.inlineByHash == nil {
		p.inlineByHash = map[string]*ast.PromptDecl{}
	}
	if _, seen := p.inlineByHash[name]; !seen {
		decl := &ast.PromptDecl{
			Name:   name,
			Body:   t.Value,
			Inline: true,
			Span:   ast.Span{Start: p.pos(t), End: p.pos(t)},
		}
		p.inlineByHash[name] = decl
		p.inlinePrompts = append(p.inlinePrompts, decl)
	}
	return name
}
