package ir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// The compiler never resolves an include against the process working
// directory — not for a prompt with no source file, and not for one whose
// recorded source is a synthetic name ("<inline>", "studio.bot") that is not
// a file: filepath.Dir of such a name is ".", the cwd. Each is refused with
// C055, even when a file of the included name sits in the cwd.
func TestCompileRefusesAnIncludeWhoseSourceIsNotAFile(t *testing.T) {
	for _, source := range []string{"", "<inline>", "studio.bot", "unparsed.bot"} {
		t.Run(strings.Trim(source, "<>")+"_", func(t *testing.T) {
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, "secret.md"), []byte("THE POD'S OWN FILE"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cwd)
			f := &ast.File{
				Schemas:   []*ast.SchemaDecl{{Name: "out"}},
				Prompts:   []*ast.PromptDecl{{Name: "p", Body: "x {{include \"secret.md\"}}", Span: ast.Span{Start: ast.Pos{File: source, Line: 1, Column: 1}}}},
				Agents:    []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{Model: "m", Output: "out", System: "p"}}},
				Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "a", Edges: []*ast.Edge{{From: "a", To: "done"}}}},
			}
			cr := Compile(f)
			var refused bool
			for _, d := range cr.Diagnostics {
				if d.Code == DiagBadPromptInclude {
					refused = true
				}
			}
			if !refused {
				t.Fatalf("source %q: no C055; diagnostics: %v", source, cr.Diagnostics)
			}
			if cr.Workflow != nil {
				if p, ok := cr.Workflow.Prompts["p"]; ok && strings.Contains(p.Body, "THE POD'S OWN FILE") {
					t.Fatalf("source %q: the working directory's file was read into the prompt", source)
				}
			}
		})
	}
}
