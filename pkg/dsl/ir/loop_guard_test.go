package ir

import (
	"testing"
)

// loopGuardSrc builds a one-loop workflow whose back-edge carries the given
// loop clause tail (e.g. ` guard: off`), so the tests below vary only the
// clause.
func loopGuardSrc(clauseTail string) string {
	return `
prompt sys:
  hi

agent worker:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  system: sys

workflow w:
  entry: worker
  worker -> done when ok
  worker -> worker as reask(2)` + clauseTail + `
  worker -> done
`
}

// TestLoopGuard_CompilesToTheLoop pins the carry: the clause's guard word
// lands on the compiled Loop, which is what the runtime's
// loopBudgetGuardEnabledFor reads.
func TestLoopGuard_CompilesToTheLoop(t *testing.T) {
	w := mustCompile(t, loopGuardSrc(" guard: off"))
	loop, ok := w.Loops["reask"]
	if !ok {
		t.Fatal("reask not compiled")
	}
	if loop.Guard != "off" {
		t.Errorf("loop.Guard = %q, want %q", loop.Guard, "off")
	}
}

// TestLoopGuard_InvalidValueIsC178: anything but on|off is a typo that
// would otherwise read as "inherit" and silently keep a guard the author
// meant to lift for this loop — the same reasoning as the workflow-level
// C133.
func TestLoopGuard_InvalidValueIsC178(t *testing.T) {
	r := compileFile(t, loopGuardSrc(" guard: maybe"))
	if got := countCode(r, DiagInvalidLoopClauseGuard); got != 1 {
		t.Fatalf("C178 count = %d, want 1 (diagnostics: %v)", got, r.Diagnostics)
	}
}

// TestLoopGuard_ConflictingClauseAcrossEdges: two edges sharing one loop
// must agree on the guard, as they must on the cap — otherwise which word
// the guard obeys would depend on the edge the run happens to cross.
func TestLoopGuard_ConflictingClauseAcrossEdges(t *testing.T) {
	src := `
prompt sys:
  hi

agent worker:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  system: sys

workflow w:
  entry: worker
  worker -> done when ok
  worker -> worker as reask(2) guard: off
  worker -> worker as reask(2)
  worker -> done
`
	r := compileFile(t, src)
	if got := countCode(r, DiagDuplicateLoop); got != 1 {
		t.Fatalf("DiagDuplicateLoop count = %d, want 1 (diagnostics: %v)", got, r.Diagnostics)
	}
}
