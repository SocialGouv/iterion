package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestExecutorDurationBoundsOnlyCorrection(t *testing.T) {
	for _, correction := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid business call", true: "correction expiry"}[correction], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			wf := validationWorkflow()
			node := wf.Nodes["my_agent"].(*ir.AgentNode)
			node.Backend = delegate.BackendClaudeCode
			wf.Schemas["MySchema"].Fields[0].EnumValues = []string{"pass"}
			calls := 0
			initialBounded := false
			correctionBounded := false
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, adverseBackendFunc(func(ctx context.Context, _ delegate.Task) (delegate.Result, error) {
				calls++
				_, bounded := ctx.Deadline()
				if calls == 1 {
					initialBounded = bounded
					verdict := "pass"
					if correction {
						verdict = "wrong"
					}
					return delegate.Result{Output: map[string]any{"summary": verdict, "score": 1}, Tokens: 1}, nil
				}
				correctionBounded = bounded
				if !bounded {
					return delegate.Result{}, errors.New("correction lacks deadline")
				}
				<-ctx.Done()
				return delegate.Result{Tokens: 1}, ctx.Err()
			}))
			st := tmpStore(t)
			ctx := context.Background()
			if _, err := st.CreateRun(ctx, "duration", wf.Name, nil); err != nil {
				t.Fatal(err)
			}
			exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			eng := New(wf, st, exec, WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()))
			rs := &runState{ctx: ctx, runID: "duration", budget: newSharedBudget(&ir.Budget{MaxDuration: "100ms"}, nil)}
			_, err := eng.executeWithResources(ctx, rs, node, nil)
			if initialBounded {
				t.Error("business call acquired correction deadline")
			}
			if correction {
				var rt *RuntimeError
				if !correctionBounded || !errors.As(err, &rt) || rt.Code != ErrCodeBudgetExceeded {
					t.Fatalf("correction bounded=%v err=%v, want BUDGET_EXCEEDED", correctionBounded, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
