package botimport

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// validateDraft compiles the generated source before anyone can write
// it to disk (same contract as botscaffold): an import that emits an
// uncompilable draft is a bug in the lowering, surfaced loudly — never
// shipped as a "draft the operator will fix". Non-error diagnostics
// are folded into the report so warnings stay visible.
func validateDraft(srcFile, botSrc string, rep *Report) error {
	pr := parser.Parse(srcFile+".bot", botSrc)
	var errs []string
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	if pr.File == nil || len(errs) > 0 {
		return fmt.Errorf("internal: generated draft does not parse (import bug, not an input problem):\n%s", strings.Join(errs, "\n"))
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			errs = append(errs, d.Error())
		} else {
			rep.placeholder(0, "compile warning on the draft: %s", d.Error())
		}
	}
	if cr.Workflow == nil || len(errs) > 0 {
		return fmt.Errorf("internal: generated draft does not compile (import bug, not an input problem):\n%s", strings.Join(errs, "\n"))
	}
	return nil
}
