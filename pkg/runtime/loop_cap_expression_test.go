package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

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
