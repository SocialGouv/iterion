package unit_test

// An external test package: it compiles the merged unit through pkg/dsl/ir
// and writes it back through pkg/dsl/unparse, which reach pkg/bundle — a
// cycle for an in-package test once the bundle walks units, not for this one.

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// The merged unit is a program: it compiles as the same file written whole
// would, and written out it carries no import line — so the flat text an
// inline launch uploads is unparse.Unparse of the merged file, nothing more.
func TestTheMergedUnitCompilesAndWritesFlat(t *testing.T) {
	files := map[string]string{
		"main.bot":      "dsl: 2\nimport \"lib/nodes.bot\"\n\nschema s:\n  ok: bool\n\nworkflow w:\n  entry: a\n  a -> done\n",
		"lib/nodes.bot": "dsl: 2\nagent a:\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"d\"\n  output: s\n",
	}
	u := unit.LoadMap(files, "main.bot")
	if u.HasErrors() {
		t.Fatalf("diagnostics: %v", u.Diagnostics)
	}
	cr := ir.Compile(u.Merged)
	if cr.Workflow == nil || cr.HasErrors() {
		t.Fatalf("the merged unit does not compile: %v", cr.Diagnostics)
	}
	if _, ok := cr.Workflow.Nodes["a"]; !ok {
		t.Fatalf("the fragment's node is not in the program: %v", cr.Workflow.Nodes)
	}
	flat := unparse.Unparse(u.Merged)
	if strings.Contains(flat, "import ") || !strings.HasPrefix(flat, "dsl: 2\n") || !strings.Contains(flat, "agent a:") {
		t.Fatalf("flat text:\n%s", flat)
	}
	if err := unparse.Verify(u.Merged, flat); err != nil {
		t.Fatalf("the flat text is not the merged program: %v", err)
	}
}
