package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

func compileBotText(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	pr := parser.Parse("x.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("fixture does not parse: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("fixture does not compile: %s", d.Error())
		}
	}
	return cr.Workflow
}

const spentLoopBot = `schema verdict:
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
    max_iterations: 20
  check -> assess
  assess -> check when not ok as retry(2)
  assess -> done when ok
`

// A loop spent with no other edge left kills the run as LOOP_EXHAUSTED —
// the code the docs and the retry policy always named — not as a missing
// edge in general; with the loop-exhaustion exit the run goes on.
func TestALoopSpentWithNoExitFailsAsLoopExhausted(t *testing.T) {
	never := func(map[string]any) (map[string]any, error) { return map[string]any{"ok": false}, nil }
	exec := newStubExecutor()
	exec.on("check", never)
	exec.on("assess", never)
	s := tmpStore(t)
	eng := New(compileBotText(t, spentLoopBot), s, exec)
	if err := eng.Run(context.Background(), "run-spent", nil); err == nil {
		t.Fatal("a spent loop with no exit finished the run")
	}
	run, err := s.LoadRun(context.Background(), "run-spent")
	if err != nil {
		t.Fatal(err)
	}
	if run.FailureCode != store.FailureLoopExhausted {
		t.Fatalf("the run died of %q, want %q (%s)", run.FailureCode, store.FailureLoopExhausted, run.Error)
	}

	exec2 := newStubExecutor()
	exec2.on("check", never)
	exec2.on("assess", never)
	s2 := tmpStore(t)
	eng2 := New(compileBotText(t, spentLoopBot+"  assess -> done\n"), s2, exec2)
	if err := eng2.Run(context.Background(), "run-exit", nil); err != nil {
		t.Fatalf("the loop-exhaustion exit did not carry the run to done: %v", err)
	}
	run2, err := s2.LoadRun(context.Background(), "run-exit")
	if err != nil {
		t.Fatal(err)
	}
	if run2.Status != store.RunStatusFinished {
		t.Fatalf("the run with an exit ended %q", run2.Status)
	}
}
