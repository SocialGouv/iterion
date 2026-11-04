package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestProbeG2RetryConsumesIterations(t *testing.T) {
	for _, maxIter := range []int{2, 3} {
		t.Run(fmt.Sprint(maxIter), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			wf := &ir.Workflow{
				Name:  "iter_probe",
				Entry: "a",
				Nodes: map[string]ir.Node{
					"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
					"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
					"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
				},
				Edges:   []*ir.Edge{{From: "a", To: "b"}, {From: "b", To: "done"}},
				Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
				Budget: &ir.Budget{MaxIterations: maxIter},
			}
			exec := newStubExecutor()
			calls := 0
			exec.on("a", func(_ map[string]any) (map[string]any, error) {
				calls++
				if calls == 1 {
					return map[string]any{"_tokens": 100, "_cost_usd": 0.01}, errors.New("stream closed")
				}
				return map[string]any{"ok": true, "_tokens": 100, "_cost_usd": 0.01}, nil
			})
			exec.on("b", func(_ map[string]any) (map[string]any, error) {
				return map[string]any{"ok": true, "_tokens": 100, "_cost_usd": 0.01}, nil
			})
			dispatch := RecoveryDispatch(func(_ context.Context, _ error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
				if prior(ErrCodeExecutionFailed) > 0 {
					return RecoveryAction{Kind: RecoveryFailTerminal}, ErrCodeExecutionFailed
				}
				return RecoveryAction{Kind: RecoveryRetrySameNode}, ErrCodeExecutionFailed
			})
			st := &checkpointCapturingStore{RunStore: tmpStore(t)}
			err := New(wf, st, exec, WithRecoveryDispatch(dispatch)).Run(context.Background(), "iter-probe", nil)
			cp := st.lastCheckpoint()
			used := -1
			if cp != nil {
				used = cp.BudgetIterationsUsed
			}
			fmt.Printf("PROBE-ITER max_iterations=%d err=%v iterations_used=%d calls_a=%d\n", maxIter, err, used, calls)
			if err != nil || used != 2 || cp.BudgetTokensUsed != 300 {
				t.Fatalf("retry consumed graph iteration: used=%d tokens=%d err=%v", used, cp.BudgetTokensUsed, err)
			}
		})
	}
}
