package ir_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/readbudget"
)

// #2779: `iterion diagram` wrote a shared loop cap whole on EVERY edge that
// carries the loop — a 50 KB cap on 100 edges was 5 MB of diagram for a
// 104 KB bot. The label quotes the cap by excerpt, as every site that names
// an author's text does.
func TestMermaid_SharedLoopCapIsAnExcerpt(t *testing.T) {
	t.Setenv(readbudget.Env, "0") // the cap's length is the probe, not the budget
	capExpr := strings.Repeat("vars.x + ", 5_000) + "1"
	var b strings.Builder
	b.WriteString("agent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\nagent c:\n  model: \"m\"\n\nworkflow w:\n  vars:\n    x: int = 1\n  entry: a\n")
	b.WriteString("  a -> b as loop1(\"" + capExpr + "\")\n")
	b.WriteString("  b -> c as loop1(\"" + capExpr + "\")\n")
	b.WriteString("  c -> a as loop1(\"" + capExpr + "\")\n")
	b.WriteString("  a -> done\n")
	wf := compileTestWorkflow(t, b.String())
	out := wf.ToMermaid(ir.MermaidCompact)
	if strings.Contains(out, capExpr) {
		t.Fatalf("the diagram carries the cap whole, once per edge")
	}
	if len(out) > 10_000 {
		t.Errorf("the diagram is %d bytes for three edges of one loop: the cap rides each whole", len(out))
	}
	if n := strings.Count(out, "loop:loop1("); n != 3 {
		t.Errorf("each edge still names its loop's cap: %d labels", n)
	}
	if !strings.Contains(out, expr.Excerpt(capExpr)) {
		t.Errorf("the cap is quoted by excerpt: %.200s", out)
	}
}
