package unparse

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Verify reports whether text — the output of Unparse(f) — reads back as the
// program f: it parses without an error and compiles to the same workflow
// with the same diagnostics (ir.SameProgram). A caller that writes the text
// somewhere the program is later read from (the studio's save path) refuses
// on an error rather than write a file that means something else than the
// document it came from.
func Verify(f *ast.File, text string) error {
	pr := parser.Parse("unparsed.bot", text)
	var errs []string
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("the serialised source does not parse: %s", strings.Join(errs, "; "))
	}
	if why := ir.SameProgram(ir.Compile(f), ir.Compile(pr.File)); why != "" {
		return fmt.Errorf("the serialised source is not the same program: %s", why)
	}
	return nil
}
