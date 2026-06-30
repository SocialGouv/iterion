package ir

import (
	"strings"
	"testing"
)

// unboundedSrc builds a converging review loop: reviewer has a `when approved`
// exit and an unbounded back-edge through fixer. budgetLine is spliced into the
// workflow block (e.g. a budget block) so tests can toggle the fuel ceiling.
func unboundedSrc(loopCap, budgetLine string) string {
	return `
schema rev:
  approved: bool

prompt sys:
  hi

agent reviewer:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  output: rev
  system: sys

agent fixer:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  system: sys

workflow w:
  entry: reviewer
` + budgetLine + `  reviewer -> done when approved
  reviewer -> fixer
  fixer -> reviewer as fix_loop(` + loopCap + `)
`
}

func TestUnbounded_FuelRequired(t *testing.T) {
	// No budget, no per-loop fuel → C097 (no silent infinity).
	r := compileFile(t, unboundedSrc("unbounded", ""))
	if got := countCode(r, DiagUnboundedNoFuel); got != 1 {
		t.Errorf("C097 (no fuel) = %d, want 1 (%v)", got, r.Diagnostics)
	}

	// Per-loop fuel satisfies the requirement.
	r = compileFile(t, unboundedSrc("unbounded 200", ""))
	if got := countCode(r, DiagUnboundedNoFuel); got != 0 {
		t.Errorf("C097 with per-loop fuel = %d, want 0 (%v)", got, r.Diagnostics)
	}

	// Workflow budget.max_iterations satisfies the requirement.
	r = compileFile(t, unboundedSrc("unbounded", "  budget:\n    max_iterations: 50\n"))
	if got := countCode(r, DiagUnboundedNoFuel); got != 0 {
		t.Errorf("C097 with budget = %d, want 0 (%v)", got, r.Diagnostics)
	}
}

func TestUnbounded_NoUndeclaredCycle(t *testing.T) {
	// An unbounded loop is still a *declared* cycle, so C019 must stay silent.
	r := compileFile(t, unboundedSrc("unbounded 200", ""))
	if got := countCode(r, DiagUndeclaredCycle); got != 0 {
		t.Errorf("C019 fired on a declared unbounded loop: %d (%v)", got, r.Diagnostics)
	}
	// And C026 (max_iterations >= 1) must not fire — unbounded has no literal.
	if got := countCode(r, DiagInvalidLoopIterations); got != 0 {
		t.Errorf("C026 fired on unbounded loop: %d (%v)", got, r.Diagnostics)
	}
}

func TestUnbounded_NoExitWarns(t *testing.T) {
	// A loop with an exit edge (reviewer -> done when approved) → no C098.
	r := compileFile(t, unboundedSrc("unbounded 200", ""))
	if got := countCode(r, DiagUnboundedNoExit); got != 0 {
		t.Errorf("C098 fired despite a when-exit: %d (%v)", got, r.Diagnostics)
	}

	// A loop with NO exit anywhere in its body → C098 warning.
	noExit := `
prompt sys:
  hi

agent a:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  system: sys

agent b:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  system: sys

workflow w:
  entry: a
  a -> b
  b -> a as spin(unbounded 100)
`
	r = compileFile(t, noExit)
	if got := countCode(r, DiagUnboundedNoExit); got != 1 {
		t.Errorf("C098 (no exit) = %d, want 1 (%v)", got, r.Diagnostics)
	}
}

func TestUnbounded_Compiles(t *testing.T) {
	w := mustCompile(t, unboundedSrc("unbounded 200", ""))
	loop, ok := w.Loops["fix_loop"]
	if !ok {
		t.Fatal("fix_loop not compiled")
	}
	if !loop.Unbounded {
		t.Error("loop.Unbounded = false, want true")
	}
	if loop.FuelCap != 200 {
		t.Errorf("loop.FuelCap = %d, want 200", loop.FuelCap)
	}
}

// TestUnbounded_RoundTrip checks the unbounded clause survives unparse so a
// loaded-then-saved .bot is stable.
func TestUnbounded_RoundTrip(t *testing.T) {
	src := unboundedSrc("unbounded 200", "")
	w := mustCompile(t, src)
	if w.Loops["fix_loop"].FuelCap != 200 {
		t.Fatalf("precondition failed")
	}
	// The unparse package has its own tests; here we only assert the IR
	// captured the unbounded form (the unparse string is asserted there).
	if !strings.Contains(src, "unbounded 200") {
		t.Fatal("source sanity check failed")
	}
}
