package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

const runtimeLoopCapSource = `vars:
  max_passes: int = 2
schema out:
  cap: int
agent work:
  model: "test-model"
  output: out
workflow w:
  worktree: none
  entry: work
  work -> work as retry(CAP)
  work -> done
`

func loopCapWorkflow(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	cr := compileBot(t, src)
	if cr.HasErrors() {
		t.Fatal(cr.Diagnostics)
	}
	return cr.Workflow
}

func TestLoopCapExpressionsResolveAtEveryCrossing(t *testing.T) {
	for _, tc := range []struct {
		name, cap string
		passes    int
		dynamic   bool
	}{
		{"literal", "1", 2, false},
		{"template", `"{{vars.max_passes}}"`, 3, false},
		{"expression", `"vars.max_passes - 1"`, 2, false},
		{"zero crossings", `"vars.max_passes - 2"`, 1, false},
		{"changed output", `"outputs.work.cap"`, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := loopCapWorkflow(t, strings.Replace(runtimeLoopCapSource, "CAP", tc.cap, 1))
			calls := 0
			exec := newStubExecutor()
			exec.on("work", func(map[string]any) (map[string]any, error) {
				calls++
				cap := 2
				if tc.dynamic && calls > 1 {
					cap = 0
				}
				return map[string]any{"cap": cap}, nil
			})
			s := tmpStore(t)
			if err := New(wf, s, exec).Run(context.Background(), "cap", nil); err != nil {
				t.Fatal(err)
			}
			if calls != tc.passes {
				t.Fatalf("executions=%d, want %d", calls, tc.passes)
			}
			r, err := s.LoadRun(context.Background(), "cap")
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != store.RunStatusFinished {
				t.Fatalf("status=%s", r.Status)
			}
		})
	}
}

func TestLoopCapExpressionReadsDottedGroupOutput(t *testing.T) {
	const source = `dsl: 2
schema out:
  cap: int
group g():
  agent work:
    model: "test-model"
    output: out
use g as r1
agent repeat:
  model: "test-model"
  output: out
workflow w:
  entry: r1.work
  worktree: none
  r1.work -> repeat
  repeat -> repeat as retry("outputs.r1.work.cap - 1")
  repeat -> done
`
	wf := loopCapWorkflow(t, source)
	exec := newStubExecutor()
	exec.on("r1.work", func(map[string]any) (map[string]any, error) {
		return map[string]any{"cap": 2}, nil
	})
	calls := 0
	exec.on("repeat", func(map[string]any) (map[string]any, error) {
		calls++
		return map[string]any{"cap": 50}, nil
	})
	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "dotted-cap", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("repeat executions=%d, want 2", calls)
	}
	for _, invalid := range []string{"outputs.r1.work.missing - 1", "outputs.r1.missing.cap - 1"} {
		bad := compileBot(t, strings.Replace(source, "outputs.r1.work.cap - 1", invalid, 1))
		if !bad.HasErrors() {
			t.Fatalf("invalid cap %q accepted", invalid)
		}
	}
}

func TestLoopCapExpressionUsesRaisedVarsOnFreshResume(t *testing.T) {
	src := strings.Replace(runtimeLoopCapSource, "workflow w:", `schema answer:
  again: bool
human gate:
  output: answer
workflow w:`, 1)
	src = strings.Replace(src, "  work -> work as retry(CAP)\n  work -> done", `  work -> gate
  gate -> work when again as retry("vars.max_passes - 1")
  gate -> done`, 1)
	wf := loopCapWorkflow(t, src)
	calls := 0
	exec := newStubExecutor()
	exec.on("work", func(map[string]any) (map[string]any, error) { calls++; return map[string]any{"cap": calls}, nil })
	s := tmpStore(t)
	ctx := context.Background()
	if err := New(wf, s, exec).Run(ctx, "resume-cap", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatal(err)
	}
	r, err := s.LoadRun(ctx, "resume-cap")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman || calls != 1 {
		t.Fatalf("initial pause: %s, calls %d", r.Status, calls)
	}
	// This is the durable input override the resume entry point resolves again;
	// the already-executed entry node is not replayed just to derive a cap.
	r.Inputs = map[string]any{"max_passes": "3"}
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		err := New(wf, s, exec).Resume(ctx, "resume-cap", map[string]any{"again": true})
		if i < 2 && !errors.Is(err, ErrRunPaused) || i == 2 && err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
	}
	r, err = s.LoadRun(ctx, "resume-cap")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFinished || calls != 3 {
		t.Fatalf("resumed status=%s, executions=%d, want 3", r.Status, calls)
	}
}

func TestInvalidLoopCapIsPersistedAsExpressionFailure(t *testing.T) {
	src := strings.Replace(runtimeLoopCapSource, "  cap: int", "  cap: json", 1)
	src = strings.Replace(src, "CAP", `"outputs.work.cap.value"`, 1)
	for _, value := range []any{nil, "bad", 1.5, -1, []any{1}} {
		t.Run(fmtLoopCap(value), func(t *testing.T) {
			wf := loopCapWorkflow(t, src)
			exec := newStubExecutor()
			exec.on("work", func(map[string]any) (map[string]any, error) {
				return map[string]any{"cap": map[string]any{"value": value}}, nil
			})
			s := tmpStore(t)
			ctx := context.Background()
			err := New(wf, s, exec).Run(ctx, "invalid-cap", nil)
			if err == nil || !strings.Contains(err.Error(), `loop "retry" cap`) {
				t.Fatalf("error=%v", err)
			}
			r, err := s.LoadRun(ctx, "invalid-cap")
			if err != nil {
				t.Fatal(err)
			}
			if r.FailureCode != store.FailureExpressionFailed || r.Status != store.RunStatusFailedResumable {
				t.Fatalf("status=%s code=%s error=%s", r.Status, r.FailureCode, r.Error)
			}
		})
	}
}

// TestALoopCapExpressionRefusesAConcatenatedString.
//
// `+` concatenates as soon as one operand is a string, so a cap written
// `outputs.pass.remaining + 1` over a field holding "3" evaluates to "31" —
// and loopCapInteger accepts a numeric string, deliberately, for the LEGACY
// single-reference template form. The two tolerances compose into an
// order-of-magnitude larger loop against the run budget, with no diagnostic.
//
// The compile-time guard cannot catch this one: it refuses an operand whose
// type is KNOWN, and an unschema'd output field has none — which is why the
// workflow here is built with the shape a compiler could not have typed.
func TestALoopCapExpressionRefusesAConcatenatedString(t *testing.T) {
	parsed, err := expr.Parse("outputs.pass.remaining + 1")
	if err != nil {
		t.Fatal(err)
	}
	wf := campaignShapedWorkflow(0)
	loop := wf.Loops["continuation"]
	loop.MaxIterations, loop.MaxIterationsExpr, loop.MaxIterationsAST = 0, "outputs.pass.remaining + 1", parsed

	passes := 0
	exec := newStubExecutor()
	exec.on("pass", func(_ map[string]any) (map[string]any, error) {
		passes++
		return map[string]any{"remaining": "3"}, nil
	})
	exec.on("gate", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"converged": false}, nil
	})
	exec.on("deliver", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"published": true}, nil
	})

	s := tmpStore(t)
	runErr := New(wf, s, exec).Run(context.Background(), "concat-cap", nil)
	if runErr == nil {
		t.Fatalf("a cap that evaluated to the string \"31\" was accepted; the loop ran %d passes where 4 was written", passes)
	}
	if !strings.Contains(runErr.Error(), `loop "continuation" cap`) || !strings.Contains(runErr.Error(), "31") {
		t.Fatalf("the refusal does not name the loop and the value it got: %v", runErr)
	}
	if passes > 2 {
		t.Errorf("passes=%d: the run kept looping on a cap it could not read", passes)
	}
	r, err := s.LoadRun(context.Background(), "concat-cap")
	if err != nil {
		t.Fatal(err)
	}
	if r.FailureCode != store.FailureExpressionFailed {
		t.Errorf("failure code = %q, want %q", r.FailureCode, store.FailureExpressionFailed)
	}
}

// A cap that READS a value keeps its numeric-string tolerance, in BOTH
// spellings — `{{outputs.x.n}}` and the un-braced `"outputs.x.n"`, which
// compileLoopCap parses into an AST whose root is a bare path. Only an
// expression that COMPUTES has to produce a number, so narrowing that path
// must not narrow either of these.
func TestACapThatOnlyReadsAValueKeepsItsNumericStringTolerance(t *testing.T) {
	for _, spelling := range []string{"template", "bare path"} {
		t.Run(spelling, func(t *testing.T) {
			wf := campaignShapedWorkflow(0)
			loop := wf.Loops["continuation"]
			loop.MaxIterations = 0
			if spelling == "template" {
				loop.MaxIterationsExpr = "{{outputs.pass.remaining}}"
				loop.MaxIterationsExprRefs = []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"pass", "remaining"}, Raw: "{{outputs.pass.remaining}}"}}
			} else {
				parsed, err := expr.Parse("outputs.pass.remaining")
				if err != nil {
					t.Fatal(err)
				}
				loop.MaxIterationsExpr, loop.MaxIterationsAST = "outputs.pass.remaining", parsed
			}

			passes := 0
			exec := newStubExecutor()
			exec.on("pass", func(_ map[string]any) (map[string]any, error) {
				passes++
				return map[string]any{"remaining": "2"}, nil
			})
			exec.on("gate", func(_ map[string]any) (map[string]any, error) {
				return map[string]any{"converged": false}, nil
			})
			exec.on("deliver", func(_ map[string]any) (map[string]any, error) {
				return map[string]any{"published": true}, nil
			})
			if err := New(wf, tmpStore(t), exec).Run(context.Background(), "reading-cap", nil); err != nil {
				t.Fatalf("a cap that only reads a value lost its numeric-string tolerance: %v", err)
			}
			if passes != 3 {
				t.Errorf("passes=%d, want 3 (the first pass plus a cap of 2 crossings)", passes)
			}
		})
	}
}

func fmtLoopCap(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestLoopCapIntegerRefusesFractionalAndOverflowingValues(t *testing.T) {
	for _, v := range []any{nil, true, -1, -1.0, 1.5, math.NaN(), math.Inf(1), math.Ldexp(1, 63), json.Number("2.5"), "bad"} {
		if n, err := loopCapInteger(v); err == nil {
			t.Errorf("%v accepted as %d", v, n)
		}
	}
	for _, v := range []any{0, int64(2), float64(2), json.Number("2.0"), "2"} {
		if _, err := loopCapInteger(v); err != nil {
			t.Errorf("%v rejected: %v", v, err)
		}
	}
}

func TestInvalidFallbackLoopCapDoesNotDefeatALaterMatchingCondition(t *testing.T) {
	wf := campaignShapedWorkflow(0)
	wf.Edges = []*ir.Edge{{From: "work", To: "work", LoopName: "retry"}, {From: "work", To: "done", Condition: "finished"}}
	wf.Loops = map[string]*ir.Loop{"retry": {Name: "retry", MaxIterationsExpr: "{{outputs.missing.cap}}", MaxIterationsExprRefs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"missing", "cap"}}}}}
	e := New(wf, tmpStore(t), newStubExecutor())
	rs := e.newRunState("unused", nil)
	edge, err := e.evaluateEdgesWithLoopsRS("work", "test", map[string]any{"finished": true}, rs)
	if err != nil || edge == nil || edge.To != "done" {
		t.Fatalf("selection=%v error=%v", edge, err)
	}
	if _, err = e.evaluateEdgesWithLoopsRS("work", "test", map[string]any{"finished": false}, rs); err == nil {
		t.Fatal("selected invalid fallback must fail")
	}
}

func TestBranchLoopCapUsesPrivateOutputsAndPropagatesFailure(t *testing.T) {
	src := strings.Replace(runtimeLoopCapSource, "  cap: int", "  cap: json", 1)
	src = strings.Replace(src, "CAP", `"outputs.work.cap.value"`, 1)
	wf := loopCapWorkflow(t, src)
	e := New(wf, tmpStore(t), newStubExecutor())
	trunk := e.newRunState("branch-cap", nil)
	trunk.outputs["work"] = map[string]any{"cap": map[string]any{"value": 9}}
	branch := e.newRunState("branch-cap", nil)
	branch.outputs["work"] = map[string]any{"cap": map[string]any{"value": 1}}
	if n, err := e.resolveLoopMaxChecked(wf.Loops["retry"], branch); err != nil || n != 1 {
		t.Fatalf("branch cap=%d err=%v", n, err)
	}
	if n, err := e.resolveLoopMaxChecked(wf.Loops["retry"], trunk); err != nil || n != 9 {
		t.Fatalf("trunk cap=%d err=%v", n, err)
	}
	branch.outputs["work"] = map[string]any{"cap": map[string]any{}}
	_, err := e.selectEdgeBranch(context.Background(), "branch-cap", "b1", "work", branch.outputs["work"], &branchResult{}, branch)
	var runtimeErr *RuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != ErrCodeExpressionFailed || !strings.Contains(runtimeErr.Message, `loop "retry" cap`) {
		t.Fatalf("branch failure=%v", err)
	}
}
