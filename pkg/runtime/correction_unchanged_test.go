package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestExecutorCorrectionStopsOnUnchangedAnswer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	wf := validationWorkflow()
	wf.Nodes["my_agent"].(*ir.AgentNode).Backend = delegate.BackendClaudeCode
	wf.Schemas["MySchema"].Fields[0].EnumValues = []string{"pass"}
	calls := 0
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, adverseBackendFunc(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		return delegate.Result{Output: map[string]any{"summary": "wrong", "score": 1}, Tokens: calls, CostUSD: 0.01}, nil
	}))
	st := tmpStore(t)
	exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg))
	eng := New(wf, st, exec, WithOutputCorrectionBudget(2))
	if err := eng.Run(context.Background(), "unchanged", nil); err == nil {
		t.Fatal("invalid output accepted")
	}
	run, err := st.LoadRun(context.Background(), "unchanged")
	if err != nil {
		t.Fatal(err)
	}
	ep := run.OutputCorrections["my_agent"]
	if calls != 2 || ep.Attempts != 1 || ep.Status != correctionStatusUnchanged {
		t.Fatalf("unchanged answer spent again: calls=%d ledger=%+v", calls, ep)
	}
}
