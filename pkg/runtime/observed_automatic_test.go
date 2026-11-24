package runtime

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

func TestObservedAutomaticRunShadowsInheritedAuthority(t *testing.T) {
	s, foreign := correctionFixture(t)
	wf := correctionEngineWorkflow()
	var paidEpoch string
	b := observedBackend(func(ctx context.Context, _ delegate.Task) (delegate.Result, error) {
		own, ok := model.OutputCorrectionOwnershipFromContext(ctx)
		if !ok || own.Authority.RunID != "child" || own.Authority.Epoch == foreign.authority.Epoch {
			t.Fatal("new Run inherited foreign execution authority")
		}
		paidEpoch = own.Authority.Epoch
		r, err := s.LoadRun(ctx, "child")
		if err != nil {
			t.Fatal(err)
		}
		if len(r.InitialUsageReservations) != 1 {
			t.Fatal("new Run missing durable initial")
		}
		return delegate.Result{Output: map[string]any{"reply": "x", "ok": true}, Tokens: 3, CostUSD: 1}, nil
	})
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, b)
	ex := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg))
	e := New(wf, s, ex)
	if err := e.Run(foreign.context(context.Background()), "child", nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(context.Background(), "child")
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range r.InitialUsageReservations {
		if res.ExecutionEpoch != paidEpoch {
			t.Fatal("receipt did not retain original claim response")
		}
	}
	if r.Checkpoint == nil || r.Checkpoint.BudgetTokensUsed != 3 {
		t.Fatalf("new run booking %+v", r.Checkpoint)
	}
}
func TestObservedAutomaticResumeRestoresBeforeCall(t *testing.T) {
	s, _ := correctionFixture(t)
	wf := correctionEngineWorkflow()
	calls := 0
	epochs := []string{}
	b := observedBackend(func(ctx context.Context, _ delegate.Task) (delegate.Result, error) {
		calls++
		own, ok := model.OutputCorrectionOwnershipFromContext(ctx)
		if !ok {
			t.Fatal("public Run/Resume did not transport authority")
		}
		epochs = append(epochs, own.Authority.Epoch)
		out := map[string]any{"reply": "x"}
		if calls == 3 {
			out["ok"] = true
		}
		return delegate.Result{Output: out, Tokens: 5, CostUSD: float64(9 + calls), CostIsSessionTotal: true, SessionID: "persisted", SessionFingerprint: "fp"}, nil
	})
	makeEngine := func(budget int) *Engine {
		reg := delegate.NewRegistry()
		reg.Register(delegate.BackendClaudeCode, b)
		ex := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
		return New(wf, s, ex, WithOutputCorrectionBudget(budget))
	}
	if err := makeEngine(0).Run(context.Background(), "correction", nil); err == nil {
		t.Fatal("invalid initial succeeded")
	}
	first := readCorrection(t, s)
	if first.Status != store.RunStatusFailedResumable || first.Checkpoint == nil || first.Checkpoint.BudgetTokensUsed != 5 {
		t.Fatalf("initial failure %+v", first)
	}
	// Genuine new Engine + external Resume, not a direct reinvocation helper.
	if err := makeEngine(1).Resume(context.Background(), "correction", nil); err != nil {
		t.Fatal(err)
	}
	final := readCorrection(t, s)
	if calls != 3 || len(epochs) != 3 || epochs[0] == epochs[1] || epochs[1] != epochs[2] {
		t.Fatalf("claim lifetime calls=%d epochs=%v", calls, epochs)
	}
	cp := final.Checkpoint
	if cp == nil || cp.BudgetTokensUsed != 15 || cp.BudgetCostUSD != 12 || cp.BudgetIterationsUsed != 1 || len(final.InitialUsageReservations) != 2 {
		t.Fatalf("resume double/lost booking %+v initial=%+v", cp, final.InitialUsageReservations)
	}
}
