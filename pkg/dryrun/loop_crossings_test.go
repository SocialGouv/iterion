package dryrun

import (
	"context"
	"strings"
	"testing"
)

// A bounded loop, cap 6, its exit conditional on the model's verdict: on
// the false pass the exit never reads true, so the loop ends by the bound
// (default 3) — the dry run's doing — where the cap of 6 is the program's.
const crossingsBot = `schema verdict:
  ok: bool

agent worker:
  model: "claude-opus-4-7"
  output: verdict

workflow sp:
  worktree: none
  sandbox: none
  entry: worker
  budget:
    max_iterations: 100
  worker -> worker when not ok as spin(6)
  worker -> done when ok
`

// The dry run's crossing bound (#1307): the loop's back-edge is declined
// after three crossings — the SIMULATION's doing, said on the pass
// (loops_cut_short) — and the death that follows (no exit on the false
// pass) is the bound's, a ceiling: not the program's word, so the report
// stays clean. The static C145 warning on the same bot is unchanged (the
// fixture compiles with no error; the warning is compile-time).
func TestALoopCutShortIsTheDryRunsDoing(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, crossingsBot), Options{})
	if err != nil {
		t.Fatal(err)
	}
	truePass, falsePass := r.Passes[0], r.Passes[1]
	// The true pass reads the exit at the first crossing and finishes; the
	// false pass is cut at the bound.
	if len(truePass.LoopsCutShort) != 0 {
		t.Fatalf("the true pass took the exit before any bound: %+v", truePass)
	}
	if len(falsePass.LoopsCutShort) != 1 || falsePass.LoopsCutShort[0] != "spin" {
		t.Fatalf("the false pass does not say the loop was cut short: %+v", falsePass)
	}
	// The bound declined the edge after three crossings — not four.
	crossings := 0
	for _, e := range falsePass.Edges {
		if e.From == "worker" && e.To == "worker" {
			crossings++
		}
	}
	if crossings != 3 {
		t.Fatalf("the loop crossed %d times, want the bound's 3: %+v", crossings, falsePass.Edges)
	}
	if !falsePass.Ceiling || falsePass.died() {
		t.Fatalf("the cut's death reads as the program's: %+v", falsePass)
	}
	if r.Failing() {
		t.Fatalf("the dry run's own bound is not the program's word: %s", r.Render())
	}
	if !r.Clean() {
		t.Fatalf("a loop cut by the bound is not held against the bot: %s", r.Render())
	}
	if out := r.Render(); !strings.Contains(out, "cut loop(s) spin") || !strings.Contains(out, "--exec-loop-crossings") {
		t.Fatalf("the render does not say the cut, nor the dial that raises it:\n%s", out)
	}
}

// The operator's dial: a negative LoopCrossings removes the bound, the loop
// runs to its own cap, and the death at the cap is the PROGRAM's again —
// died, not clean.
func TestTheCrossingDialLiftedRunsTheLoopToItsCap(t *testing.T) {
	r, err := Run(context.Background(), compileBot(t, crossingsBot), Options{LoopCrossings: -1})
	if err != nil {
		t.Fatal(err)
	}
	falsePass := r.Passes[1]
	if len(falsePass.LoopsCutShort) != 0 {
		t.Fatalf("the bound was lifted and a loop is still said cut: %+v", falsePass)
	}
	if falsePass.Ceiling || !falsePass.died() {
		t.Fatalf("a loop spent at its own cap with no exit is the program's death: %+v", falsePass)
	}
	if !r.Failing() || r.Clean() {
		t.Fatalf("the program's death at the cap must fail the report: %s", r.Render())
	}
}
