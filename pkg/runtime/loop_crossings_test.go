package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A bounded loop the simulation keeps crossing: cap 20, the stub always
// answers not ok, so a production engine runs the loop to its cap and dies
// of LOOP_EXHAUSTED — the program's word.
const crossingsLoopBot = `schema verdict:
  ok: bool

agent check:
  model: "m"
  output: verdict

judge assess:
  model: "m"
  output: verdict

workflow w:
  worktree: none
  sandbox: none
  entry: check
  budget:
    max_iterations: 60
  check -> assess
  assess -> check when not ok as retry(20)
  assess -> done when ok
`

// A simulation's crossing bound (Simulation.LoopCrossings, a dry run's
// #1307) declines a bounded loop's back-edge past the bound — the
// SIMULATION's doing: the decline is said by a budget_warning naming its
// reason, and the death that follows (no other edge matches) carries a
// LoopDeclined that reads as a ceiling (CeilingReason), never as the
// program's LOOP_EXHAUSTED-of-the-program. C145's static reading of the
// exit is compile-time and untouched.
func TestASimulationCrossingBoundDeclinesTheLoopAsItsOwn(t *testing.T) {
	never := func(map[string]any) (map[string]any, error) { return map[string]any{"ok": false}, nil }
	exec := newStubExecutor()
	exec.on("check", never)
	exec.on("assess", never)
	s := tmpStore(t)
	eng := New(compileBotText(t, crossingsLoopBot), s, exec, WithSimulation(Simulation{LoopCrossings: 2}))
	err := eng.Run(context.Background(), "run-bound", nil)
	if err == nil {
		t.Fatal("a loop cut at the bound with no exit taken did not end the run")
	}
	var d *LoopDeclined
	if !errors.As(err, &d) || d.Reason != SimulationLoopCrossingsDecline {
		t.Fatalf("the death does not carry the simulation's decline: %v (%v)", err, d)
	}
	if !CeilingReason(d.Reason) {
		t.Fatalf("the simulation's decline %q is not a ceiling: the death would read as the program's", d.Reason)
	}
	if CeilingReason("loop_cap") {
		t.Fatal("a bounded loop's cap reads as a ceiling: the program's word would be lost")
	}
	run, lerr := s.LoadRun(context.Background(), "run-bound")
	if lerr != nil {
		t.Fatalf("load run: %v", lerr)
	}
	if run.FailureCode != store.FailureLoopExhausted {
		t.Fatalf("the run died of %q, want %q (%s)", run.FailureCode, store.FailureLoopExhausted, run.Error)
	}
	events, lerr := s.LoadEvents(context.Background(), "run-bound")
	if lerr != nil {
		t.Fatalf("load events: %v", lerr)
	}
	said := false
	for _, evt := range events {
		if evt.Type == store.EventBudgetWarning && evt.Data["reason"] == SimulationLoopCrossingsDecline {
			said = true
		}
	}
	if !said {
		t.Fatal("the decline was said by no budget_warning: a reader of the run could not tell the bound's doing")
	}
}

// A loop cap tighter than the simulation's bound stays the program's word:
// the cap skip fires first (loop_cap, no ceiling), so the death reads as
// the program's — the C145 shape, whatever the dry run's dial says.
func TestALoopCapTighterThanTheBoundStaysThePrograms(t *testing.T) {
	src := replaceLoopCap(crossingsLoopBot, "retry(20)", "retry(2)")
	never := func(map[string]any) (map[string]any, error) { return map[string]any{"ok": false}, nil }
	exec := newStubExecutor()
	exec.on("check", never)
	exec.on("assess", never)
	s := tmpStore(t)
	eng := New(compileBotText(t, src), s, exec, WithSimulation(Simulation{LoopCrossings: 5}))
	err := eng.Run(context.Background(), "run-cap", nil)
	if err == nil {
		t.Fatal("a spent loop with no exit finished the run")
	}
	var d *LoopDeclined
	if !errors.As(err, &d) || d.Reason != "loop_cap" {
		t.Fatalf("the death does not carry the program's cap decline: %v (%v)", err, d)
	}
	if CeilingReason(d.Reason) {
		t.Fatal("the program's cap decline reads as a ceiling")
	}
}

// A production engine (LoopCrossings zero) runs the loop to its cap: the
// bound is the simulation's, no production launch carries it.
func TestAProductionEngineHasNoCrossingBound(t *testing.T) {
	never := func(map[string]any) (map[string]any, error) { return map[string]any{"ok": false}, nil }
	exec := newStubExecutor()
	exec.on("check", never)
	exec.on("assess", never)
	s := tmpStore(t)
	eng := New(compileBotText(t, replaceLoopCap(crossingsLoopBot, "retry(20)", "retry(3)")), s, exec)
	err := eng.Run(context.Background(), "run-prod", nil)
	if err == nil {
		t.Fatal("the run finished though the loop was spent with no exit")
	}
	var d *LoopDeclined
	if !errors.As(err, &d) || d.Reason != "loop_cap" {
		t.Fatalf("the death does not carry the program's cap decline: %v (%v)", err, d)
	}
}

func replaceLoopCap(src, from, to string) string {
	if !strings.Contains(src, from) {
		panic("fixture drift: " + from)
	}
	return strings.Replace(src, from, to, 1)
}
