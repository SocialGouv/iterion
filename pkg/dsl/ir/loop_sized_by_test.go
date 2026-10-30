package ir

import (
	"testing"
)

// loopSizedBySrc builds a one-loop workflow whose back-edge carries the
// given loop clause tail (e.g. ` sized_by "outputs.worker.pending"`),
// so the tests below vary only the clause. The worker's output schema
// gives `pending` a known int type where a test needs one.
func loopSizedBySrc(clauseTail string) string {
	return `
prompt sys:
  hi

schema probeout:
  ok: bool
  pending: int

agent worker:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  system: sys
  output: probeout

workflow w:
  entry: worker
  worker -> done when ok
  worker -> worker as reask(2)` + clauseTail + `
  worker -> done
`
}

// TestLoopSizedBy_CompilesToTheLoop pins the carry: the clause's signal
// lands on the compiled Loop, source and AST, which is what the
// runtime's markLoopBudget / loopBudgetShortfall evaluate (#2677).
func TestLoopSizedBy_CompilesToTheLoop(t *testing.T) {
	w := mustCompile(t, loopSizedBySrc(` sized_by "outputs.worker.pending"`))
	loop, ok := w.Loops["reask"]
	if !ok {
		t.Fatal("reask not compiled")
	}
	if loop.SizedByExpr != "outputs.worker.pending" {
		t.Errorf("loop.SizedByExpr = %q, want %q", loop.SizedByExpr, "outputs.worker.pending")
	}
	if loop.SizedByAST == nil {
		t.Fatal("loop.SizedByAST is nil — the runtime would price unscaled")
	}
}

// TestLoopSizedBy_InvalidSignalIsC325: a signal that does not parse, or
// one provably not numeric, must be refused at compile time — at run
// time an unreadable signal prices unscaled, so a typo would otherwise
// pass silently as "no proration".
func TestLoopSizedBy_InvalidSignalIsC325(t *testing.T) {
	for name, tail := range map[string]string{
		"unparseable expression":           ` sized_by "outputs.worker.pending >"`,
		"template braces":                  ` sized_by "{{outputs.worker.pending}}"`,
		"a namespace outside vars/outputs": ` sized_by "run.max_tokens"`,
	} {
		t.Run(name, func(t *testing.T) {
			r := compileFile(t, loopSizedBySrc(tail))
			if got := countCode(r, DiagInvalidLoopSizedBy); got != 1 {
				t.Fatalf("C325 count = %d, want 1 (diagnostics: %v)", got, r.Diagnostics)
			}
		})
	}
}

// TestLoopSizedBy_ConflictingClauseAcrossEdges: two edges sharing one
// loop must agree on the signal, as they must on the cap and the guard —
// otherwise which traversal the guard prices would depend on the edge
// the run happens to cross.
func TestLoopSizedBy_ConflictingClauseAcrossEdges(t *testing.T) {
	src := `
prompt sys:
  hi

schema probeout:
  ok: bool
  pending: int

agent worker:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  system: sys
  output: probeout

workflow w:
  entry: worker
  worker -> done when ok
  worker -> worker as reask(2) sized_by "outputs.worker.pending"
  worker -> worker as reask(2) sized_by "outputs.worker.other"
  worker -> done
`
	r := compileFile(t, src)
	if got := countCode(r, DiagDuplicateLoop); got != 1 {
		t.Fatalf("DiagDuplicateLoop count = %d, want 1 (diagnostics: %v)", got, r.Diagnostics)
	}
}
