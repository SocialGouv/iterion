// Package canon gives a `.bot` file its canonical form: the text the studio
// saves (pkg/dsl/unparse), proven to read as the same program AND to carry
// the same comments before it is handed back (unparse.Verify), written on
// the file's own bytes — its BOM and its line endings kept — and refused by
// name when the writer cannot produce it without changing the file.
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
// as it is: one that does not parse, or one the proof refuses — which now
// includes a comment the round trip would lose or move off its
// declaration, since the writer puts each one back where it was written
// and unparse.Verify compares them (#1282).
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
