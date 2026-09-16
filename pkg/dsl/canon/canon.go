// Package canon gives a `.bot` file its canonical form: the text the studio
// saves (pkg/dsl/unparse), proven to read as the same program before it is
// handed back (unparse.Verify), written on the file's own bytes — its BOM
// and its line endings kept — and refused by name when the writer would
// change the file in a way the proof cannot see: a comment moved away from
// what it described.
package canon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/internal/rewrite"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// ErrRefused marks a file that cannot be given its canonical form without
// changing it; the error says why.
var ErrRefused = errors.New("canonical form refused")

// Bytes is the canonical form of src, the file named name: the same bytes
// when the file already has it, else the writer's text mapped back onto the
// file's own BOM and line endings. A refusal (ErrRefused) leaves the file
// as it is: one that does not parse; one whose comments the writer would
// move — it keeps the comments that lead the file, above the `dsl:` header,
// the imports and the first declaration, and no other (a comment inside a
// block never reaches the AST, #1282); one the proof refuses.
func Bytes(name string, src []byte) ([]byte, error) {
	norm := rewrite.Normalize(src)
	pr := parser.Parse(name, norm.Text)
	var errs []string
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%w: does not parse: %s", ErrRefused, strings.Join(errs, "; "))
	}
	if c := commentAfterHead(name, norm.Text); c.line > 0 {
		return nil, fmt.Errorf("%w: a comment at line %d follows %s (line %d): the writer keeps only the comments that lead the file and would move or lose it — move it above the `dsl:` header, the imports and the first declaration, or leave the file as it is", ErrRefused, c.line, c.after, c.headLine)
	}
	text, err := provenText(pr.File)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot be rewritten without changing the program: %v", ErrRefused, err)
	}
	if text == norm.Text {
		return src, nil
	}
	// The whole text is one edit, mapped back onto the original bytes: the
	// BOM stays, and every line ending follows the file's own.
	return norm.MapBack(src, []rewrite.Edit{{Start: 0, End: len(norm.Text), Repl: text}}), nil
}

// provenText is the writer's text for f, proven to read as the same
// program (unparse.Verify) — or the reason it cannot be. From parsed text
// the proof holds by the round-trip the writer keeps; it is what stands
// between a document the writer cannot carry and a file that means
// something else.
func provenText(f *ast.File) (string, error) {
	text := unparse.Unparse(f)
	if err := unparse.Verify(f, text); err != nil {
		return "", err
	}
	return text, nil
}

// misplaced is a comment the writer would move: its line, what it follows
// and the line of that.
type misplaced struct {
	line     int
	after    string
	headLine int
}

// commentAfterHead finds the first comment the writer would not keep in
// place. The comments that lead the file — before the `dsl:` header, the
// import lines and the first declaration — are written first, where they
// are; a comment after any of those (on the header's own line, between two
// imports, above the first declaration once a header or an import has
// gone by, or anywhere past the first declaration) is hoisted to the head,
// away from what it described.
func commentAfterHead(name, src string) misplaced {
	tokens := parser.NewLexer(name, src).All()
	headLine := 0 // the line of the header or the last import consumed
	i := 0
head:
	for i < len(tokens) {
		switch tokens[i].Type {
		case parser.TokenNewline:
			i++
		case parser.TokenComment:
			if headLine > 0 {
				return misplaced{line: tokens[i].Line, after: "the `dsl:` header or an import", headLine: headLine}
			}
			i++
		case parser.TokenDSL, parser.TokenImport:
			headLine = tokens[i].Line
			for i < len(tokens) && tokens[i].Type != parser.TokenNewline && tokens[i].Type != parser.TokenEOF {
				if tokens[i].Type == parser.TokenComment {
					return misplaced{line: tokens[i].Line, after: "the `dsl:` header or an import", headLine: headLine}
				}
				i++
			}
		default:
			break head
		}
	}
	if i >= len(tokens) || tokens[i].Type == parser.TokenEOF {
		return misplaced{}
	}
	first := tokens[i].Line
	for _, t := range tokens[i:] {
		if t.Type == parser.TokenComment {
			return misplaced{line: t.Line, after: "the first declaration", headLine: first}
		}
	}
	return misplaced{}
}
