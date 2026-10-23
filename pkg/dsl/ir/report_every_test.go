package ir

import (
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The compiler reports every diagnostic, past the thousand of a code each
// reader of a source reports: its callers compare its whole list — fix
// proves an edit by the diagnostics it leaves, an import refuses on every
// error but one code — so a cap would turn a count past it into a false
// verdict. It draws at most a few per declaration, which the read budget
// bounds.
func TestTheCompilerReportsEveryDiagnostic(t *testing.T) {
	var b strings.Builder
	for i := 0; i < parser.MaxReported+500; i++ {
		fmt.Fprintf(&b, "agent a%d:\n  model: \"m\"\n  output: nosuch\n", i)
	}
	b.WriteString("\nworkflow w:\n  entry: a0\n  a0 -> done\n")
	pr := parsertest.Parse("many.bot", b.String())
	if len(pr.Diagnostics) != 0 {
		t.Fatalf("the source does not parse: %v", pr.Diagnostics[0])
	}
	n := 0
	for _, d := range Compile(pr.File).Diagnostics {
		if d.Code == DiagUnknownSchema {
			n++
		}
	}
	if n != parser.MaxReported+500 {
		t.Errorf("%d nodes referencing an unknown schema draw %d C002, want every one", parser.MaxReported+500, n)
	}
}
