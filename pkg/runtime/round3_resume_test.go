package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Same shape as TestR2ExecutorCorrectionAfterAndResumeRaise, but the model
// repeats ONE invalid answer before the resume, and would answer validly to
// the re-ask after it.
func TestR3UnchangedEpisodeAndResumeRaise(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	wf := validationWorkflow()
	n := wf.Nodes["my_agent"].(*ir.AgentNode)
	n.Backend = delegate.BackendClaudeCode
	wf.Schemas["MySchema"].Fields[0].EnumValues = []string{"pass"}
	calls := 0
	resumed := false
	backend := adverseBackendFunc(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		summary := "invalid"
		if resumed {
			summary = "invalid-after-resume"
			if calls >= 4 {
				summary = "pass"
			}
		}
		return delegate.Result{Output: map[string]any{"summary": summary, "score": 1}, Tokens: 1}, nil
	})
	registry := delegate.NewRegistry()
	registry.Register(delegate.BackendClaudeCode, backend)
	st := tmpStore(t)
	exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(registry))
	ctx := context.Background()
	err := New(wf, st, exec, WithOutputCorrectionBudget(1)).Run(ctx, "raise", nil)
	run, _ := st.LoadRun(ctx, "raise")
	ep := run.OutputCorrections["my_agent"]
	if err == nil || calls != 2 || ep.Status != "unchanged" || ep.Budget != 1 {
		t.Fatalf("initial calls=%d err=%v ep=%+v", calls, err, ep)
	}
	resumed = true
	before := calls
	err = New(wf, st, exec, WithOutputCorrectionBudget(3)).Resume(ctx, "raise", nil)
	run, _ = st.LoadRun(ctx, "raise")
	ep = run.OutputCorrections["my_agent"]
	if err != nil || calls-before != 2 || run.Status != "finished" || ep.Status != "succeeded" || ep.Budget != 3 || ep.Attempts != 2 {
		t.Fatalf("resume calls=%d status=%s err=%v ep=%+v", calls-before, run.Status, err, ep)
	}
}
