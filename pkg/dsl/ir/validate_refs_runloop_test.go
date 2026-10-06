package ir

import (
	"strings"
	"testing"
)

// #1506: the run and loop namespaces inside a compute `expr:` and inside an
// edge's quoted `when "..."` flow through the same ref walker as every other
// namespace, so a typo'd run member or an undeclared loop is a compile-time
// warning instead of a silent nil at run time. The when-walk keeps only the
// run/loop kinds — field conditions stay validateConditionFields' territory.
const runLoopRefsSrc = `
schema verdict:
  ok: bool

schema noise:
  noise: string

agent a:
  model: "m"
  user: p
  output: verdict

compute c:
  output: noise
  expr:
    noise: "run.tree_nose"

workflow w:
  worktree: none
  sandbox: none
  entry: a
  budget:
    max_iterations: 20
  a -> c with {
    ok: "{{outputs.a.ok}}"
  }
  c -> a when "run.nope == 1" as l(3)
  c -> done when ok
`

// The compute expression's run ref is a typo (tree_nose is not a run member)
// and the edge `when` names a member the run namespace has not: each fires
// C153 once, worded with where it sits.
func TestRunRefsInComputeExprAndEdgeWhenAreChecked(t *testing.T) {
	r := compileText(t, runLoopRefsSrc)
	got := 0
	for _, d := range r.Diagnostics {
		if d.Code == DiagUnknownRunMember {
			got++
			t.Logf("C153: %s", d.Message)
		}
	}
	if got < 2 {
		t.Fatalf("expected the compute-expr typo and the when-expr member to both fire C153, got %d — diagnostics:\n%s",
			got, diagnosticsDump(r))
	}
	var computeHit, whenHit bool
	for _, d := range r.Diagnostics {
		if d.Code != DiagUnknownRunMember {
			continue
		}
		switch {
		case strings.Contains(d.Message, `expr "noise"`) && strings.Contains(d.Message, "run.tree_nose"):
			computeHit = true
		case strings.Contains(d.Message, "when") && strings.Contains(d.Message, "run.nope"):
			whenHit = true
		}
	}
	if !computeHit || !whenHit {
		t.Fatalf("expected one C153 for the compute expr (run.tree_nose) and one for the when (run.nope), computeHit=%v whenHit=%v — diagnostics:\n%s",
			computeHit, whenHit, diagnosticsDump(r))
	}
}

// The loop namespace inside a compute expression names a loop no edge
// declares: C147, like the prompt form.
func TestLoopRefInComputeExprIsChecked(t *testing.T) {
	src := strings.Replace(runLoopRefsSrc, "run.tree_nose", "loop.TYPO.iteration", 1)
	r := compileText(t, src)
	for _, d := range r.Diagnostics {
		if d.Code == DiagUnknownLoopRef && strings.Contains(d.Message, "TYPO") {
			return
		}
	}
	t.Fatalf("expected C147 for the undeclared loop in the compute expr — diagnostics:\n%s", diagnosticsDump(r))
}

// A `when` on the edge that DECLARES the loop resolves: w.Loops is fully
// populated before the ref walk runs, so the self-edge's own loop is not an
// unknown-loop C147.
func TestWhenOnItsOwnLoopEdgeDoesNotFireC147(t *testing.T) {
	src := strings.Replace(runLoopRefsSrc, `when "run.nope == 1" as l(3)`, `when "loop.l.iteration > 0" as l(3)`, 1)
	r := compileText(t, src)
	for _, d := range r.Diagnostics {
		if d.Code == DiagUnknownLoopRef {
			t.Fatalf("the declaring edge's own when must not fire C147: %s", d.Message)
		}
	}
}

// diagnosticsDump renders one line per diagnostic for failure messages.
func diagnosticsDump(r *CompileResult) string {
	var b strings.Builder
	for _, d := range r.Diagnostics {
		b.WriteString(string(d.Code))
		b.WriteString(" ")
		b.WriteString(d.Message)
		b.WriteString("\n")
	}
	return b.String()
}
