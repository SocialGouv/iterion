package runtime

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

type probeHeavyInvalid struct{ calls int }

func (b *probeHeavyInvalid) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.calls++
	return delegate.Result{Output: map[string]any{"verdict": "wrong"}, BackendName: delegate.BackendClaudeCode, Tokens: 95}, nil
}

// A correction must not start once the node's spend sits at the run's hard
// boundary (90% of max_tokens).
func TestR2ExecutorCorrectionSpendGuard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	wf := &ir.Workflow{
		Name: "probe_l", Entry: "worker",
		Nodes: map[string]ir.Node{
			"worker": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}, LLMFields: ir.LLMFields{Backend: delegate.BackendClaudeCode}, SchemaFields: ir.SchemaFields{OutputSchema: "S"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "worker", To: "done"}},
		Schemas: map[string]*ir.Schema{"S": {Name: "S", Fields: []*ir.SchemaField{{Name: "verdict", Type: ir.FieldTypeString, EnumValues: []string{"pass"}}}}},
		Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
		Budget: &ir.Budget{MaxTokens: 100, CapImposed: true},
	}
	b := &probeHeavyInvalid{}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, b)
	st := tmpStore(t)
	exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	err := New(wf, st, exec, WithOutputCorrectionBudget(2)).Run(context.Background(), "probe-l", nil)
	r, _ := st.LoadRun(context.Background(), "probe-l")
	tok := 0
	if r.Checkpoint != nil {
		tok = r.Checkpoint.BudgetTokensUsed
	}
	if b.calls != 1 || tok != 95 || r.FailureCode != ErrCodeBudgetExceeded {
		t.Fatalf("correction spent beyond guard: calls=%d tokens=%d code=%s", b.calls, tok, r.FailureCode)
	}
	t.Logf("calls=%d booked tokens=%d/100 code=%s err=%v", b.calls, tok, r.FailureCode, err)
	for k, ep := range r.OutputCorrections {
		if ep.Status != "spend_blocked" || ep.Attempts != 0 {
			t.Errorf("incorrect blocked ledger: %+v", ep)
		}
		t.Logf("ledger[%s] attempts=%d status=%s", k, ep.Attempts, ep.Status)
	}
}

func TestR2ExecutorCorrectionAfterAndResumeRaise(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	wf := validationWorkflow()
	n := wf.Nodes["my_agent"].(*ir.AgentNode)
	n.Backend = delegate.BackendClaudeCode
	wf.Schemas["MySchema"].Fields[0].EnumValues = []string{"pass"}
	calls := 0
	backend := adverseBackendFunc(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		summary := fmt.Sprint("invalid-", calls)
		if calls == 4 {
			summary = "pass"
		}
		return delegate.Result{Output: map[string]any{"summary": summary, "score": 1}, Tokens: 1}, nil
	})
	registry := delegate.NewRegistry()
	registry.Register(delegate.BackendClaudeCode, backend)
	st := tmpStore(t)
	exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(registry))
	ctx := context.Background()
	eng := New(wf, st, exec, WithOutputCorrectionBudget(1))
	if err := eng.Run(ctx, "raise", nil); err == nil {
		t.Fatal("invalid output accepted")
	}
	run, err := st.LoadRun(ctx, "raise")
	if err != nil {
		t.Fatal(err)
	}
	ep := run.OutputCorrections["my_agent"]
	if ep.Attempts != 1 || ep.Status != "exhausted" {
		t.Fatalf("After did not persist exhaustion: %+v", ep)
	}
	assertPhase := func(want string) {
		t.Helper()
		events, err := st.LoadEvents(ctx, "raise")
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == store.EventNodeRecovery && event.Data["phase"] == want {
				return
			}
		}
		t.Errorf("missing lifecycle event %s", want)
	}
	assertPhase("exhausted")
	eng = New(wf, st, exec, WithOutputCorrectionBudget(2))
	if err := eng.Resume(ctx, "raise", nil); err != nil {
		t.Fatalf("raised allowance rejected: calls=%d err=%v", calls, err)
	}
	run, err = st.LoadRun(ctx, "raise")
	if err != nil {
		t.Fatal(err)
	}
	ep = run.OutputCorrections["my_agent"]
	if calls != 4 || ep.Attempts != 2 || ep.Budget != 2 || ep.Status != "succeeded" {
		t.Fatalf("raised/settled ledger: calls=%d episode=%+v", calls, ep)
	}
	assertPhase("succeeded")
}
