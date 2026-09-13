package parser

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// inlineNameHexLen is how many hex digits of the body's SHA-256 an inline
// prompt's name carries. Twelve keep the name readable; two DIFFERENT bodies
// under one prefix (a construction, at 2^-48 per pair by chance) never share
// it — the second takes a longer prefix (promptRef). A variable so a test
// can force the collision.
var inlineNameHexLen = 12

// InlinePromptName is the name of the prompt a text written inline on a
// referencing property becomes: derived from the body alone, so it is the
// same whatever node references it, wherever the node sits (a group member
// included), and whatever the node is renamed to — and two references to
// the same text name the same prompt.
func InlinePromptName(body string) string {
	return inlinePromptName(body, inlineNameHexLen)
}

func inlinePromptName(body string, hexLen int) string {
	sum := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(sum[:])
	if hexLen > len(digest) {
		hexLen = len(digest)
	}
	return "_inline_" + digest[:hexLen]
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
	if p.inlineByHash == nil {
		p.inlineByHash = map[string]*ast.PromptDecl{}
	}
	// The same text is one prompt; a different text under the same prefix
	// takes a longer one, so a name never stands for two bodies.
	hexLen := inlineNameHexLen
	name := inlinePromptName(t.Value, hexLen)
	for {
		decl, seen := p.inlineByHash[name]
		if !seen {
			break
		}
		if decl.Body == t.Value {
			return name
		}
		if hexLen >= sha256.Size*2 {
			break // the full digest collides on two different texts: not a thing
		}
		hexLen += 4
		name = inlinePromptName(t.Value, hexLen)
	}
	decl := &ast.PromptDecl{
		Name:   name,
		Body:   t.Value,
		Inline: true,
		Span:   ast.Span{Start: p.pos(t), End: p.pos(t)},
	}
	p.inlineByHash[name] = decl
	p.inlinePrompts = append(p.inlinePrompts, decl)
	return name
}
