package author

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// A document is the .bot it writes whatever order it lists its inline
// prompts in. The writer puts `system:` before `user:` and a group's nodes
// after the others; where nothing compiles to a workflow — a main whose
// imports are read apart from it, a fragment — the span-free mirror is the
// oracle, and it read that order as a difference (E054, no written form).
func TestAnInlinePromptsOrderIsNoDifference(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "every-kind-v2.written.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		"a main with imports, user before system": "dsl: 2\nimports: [lib/schemas.bot]\nnodes:\n  - agent: a\n    model: m\n    output: s\n    user: Zeta user text\n    system: Alpha system text\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n",
		"a fragment, its groups first":            "dsl: 2\ngroups:\n  - group: g\n    nodes:\n      - agent: look\n        model: m\n        user: Look at it\nnodes:\n  - agent: survey\n    model: m\n    user: Inline text\n",
		"the writer's own every-kind document":    string(golden),
	} {
		t.Run(name, func(t *testing.T) {
			res := Parse("x.bot.yaml", []byte(doc))
			if res.HasErrors() {
				t.Fatalf("the document does not read: %v", res.Diagnostics)
			}
			if err := unparse.Verify(res.File, unparse.Unparse(res.File)); err != nil {
				t.Fatalf("the .bot the document writes is refused as another document: %v", err)
			}
		})
	}
}
