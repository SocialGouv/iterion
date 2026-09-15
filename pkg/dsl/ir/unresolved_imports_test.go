package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A file that still carries `import` lines is not a program: its fragments
// were never merged in. Compiling it alone would only report what the main
// happens to reference, so the compiler refuses it closed (C030), naming
// the fragments, with no workflow.
func TestAFileWithUnresolvedImportsDoesNotCompile(t *testing.T) {
	src := "import \"lib/a.bot\"\nimport \"lib/b.bot\"\n\nagent a:\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"d\"\n\nworkflow w:\n  entry: a\n  a -> done\n"
	res := parser.Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("fixture: %v", res.Diagnostics)
	}
	cr := Compile(res.File)
	if cr.Workflow != nil {
		t.Fatalf("a file with unresolved imports compiled to a workflow")
	}
	if len(cr.Diagnostics) != 1 || cr.Diagnostics[0].Code != DiagUnresolvedImports || cr.Diagnostics[0].Severity != SeverityError {
		t.Fatalf("diagnostics: %v", cr.Diagnostics)
	}
	if msg := cr.Diagnostics[0].Message; !strings.Contains(msg, "lib/a.bot, lib/b.bot") || !strings.Contains(msg, "as a unit") {
		t.Fatalf("message: %s", msg)
	}
	// The same file with the imports cleared — what the unit loader hands
	// over — compiles as before.
	res.File.Imports = nil
	if cr := Compile(res.File); cr.Workflow == nil || cr.HasErrors() {
		t.Fatalf("the merged form does not compile: %v", cr.Diagnostics)
	}
}
