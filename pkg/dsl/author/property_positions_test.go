package author

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestAPropertyDiagnosticIsPositionedOnTheDocumentsKey: the span of a
// property line is remapped onto the YAML twin like every other position
// (spans walks every ast.Pos the AST reaches, the per-property table's
// included), so a compile diagnostic about a value written in the document
// points at the document's own key — the node's key for the node's
// property, the workflow's for the workflow's. The span's END is not
// claimed on a document: the mapper knows where a YAML node starts, not
// where its text ends, and an end it cannot know would land on the value's
// first rune.
func TestAPropertyDiagnosticIsPositionedOnTheDocumentsKey(t *testing.T) {
	src := "dsl: 2\n" +
		"prompts:\n" +
		"  ask: Say hello.\n" +
		"nodes:\n" +
		"  - agent: hello\n" +
		"    system: ask\n" +
		"    compress: bogus\n" +
		"workflow:\n" +
		"  name: hello\n" +
		"  entry: hello\n" +
		"  compress: zz\n" +
		"  edges:\n" +
		"    - hello -> done\n"
	res := Parse("twin.bot.yaml", []byte(src))
	if errs := errorsOf(res); len(errs) > 0 {
		t.Fatalf("refused: %v", errs)
	}
	wantNode, wantWorkflow := lineOf(src, "    compress: bogus"), lineOf(src, "  compress: zz")
	seen := map[string]bool{}
	for _, d := range ir.Compile(res.File).Diagnostics {
		if d.Code != ir.DiagInvalidCompress {
			continue
		}
		want := wantWorkflow
		if d.NodeID == "hello" {
			want = wantNode
		}
		seen[d.NodeID] = true
		if d.File != "twin.bot.yaml" || d.Line != want || d.Property != "compress" {
			t.Errorf("C102 on node %q: at %s:%d about %q, want twin.bot.yaml:%d about compress", d.NodeID, d.File, d.Line, d.Property, want)
		}
		if d.EndLine != 0 || d.EndColumn != 0 {
			t.Errorf("C102 on node %q claims a span end %d:%d on the document, whose mapper knows no end", d.NodeID, d.EndLine, d.EndColumn)
		}
	}
	if !seen["hello"] || !seen[""] {
		t.Fatalf("expected C102 on the node and on the workflow, got %v", seen)
	}
}
