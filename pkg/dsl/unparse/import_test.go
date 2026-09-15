package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The writer puts the imports back where the parser reads them — after the
// header in profile 2, after the comments in profile 1 — and the guard holds
// the text to the document, imports included.
func TestImportsAreWrittenAtTheHead(t *testing.T) {
	for name, src := range map[string]string{
		"profile 2": "## a bot\n\ndsl: 2\nimport \"lib/a.bot\"\nimport \"lib/b.bot\"\n\nagent a:\n  description: \"d\"\n",
		"profile 1": "## a bot\n\nimport \"lib/a.bot\"\n\nagent a:\n  description: \"d\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			res := parser.Parse("x.bot", src)
			if len(res.Diagnostics) != 0 {
				t.Fatalf("fixture: %v", res.Diagnostics)
			}
			out := Unparse(res.File)
			if out != src {
				t.Fatalf("written:\n%s\nwant:\n%s", out, src)
			}
			if err := Verify(res.File, out); err != nil {
				t.Fatalf("Verify: %v", err)
			}
			// Dropping an import is a change the guard sees.
			if err := Verify(res.File, strings.Replace(out, "import \"lib/a.bot\"\n", "", 1)); err == nil {
				t.Fatalf("a text missing an import passed the guard")
			}
		})
	}
}
