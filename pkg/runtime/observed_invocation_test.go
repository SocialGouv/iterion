package runtime

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

type observedBackend func(context.Context, delegate.Task) (delegate.Result, error)

func (f observedBackend) Execute(c context.Context, t delegate.Task) (delegate.Result, error) {
	return f(c, t)
}
func TestObservedInvocationAccountingMatrix(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		tokens, cost, unreported                     int
		initialValid, roundValid                     bool
		fail                                         bool
		wantCalls, wantIterations, wantUnpricedNodes int
	}{
		{name: "initial valid zero", initialValid: true, wantCalls: 1, wantIterations: 1},
		{name: "initial zero then success zero", roundValid: true, wantCalls: 2, wantIterations: 1},
		{name: "initial zero then failure zero", fail: true, wantCalls: 2},
		{name: "unreported only", unreported: 1, fail: true, wantCalls: 2},
		{name: "known session", tokens: 5, cost: 10, roundValid: true, wantCalls: 2, wantIterations: 1},
		{name: "unknown price", tokens: 5, roundValid: true, wantCalls: 2, wantIterations: 1, wantUnpricedNodes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, x := correctionFixture(t)
			wf := correctionEngineWorkflow()
			calls := 0
			b := observedBackend(func(ctx context.Context, task delegate.Task) (delegate.Result, error) {
				calls++
				r := readCorrection(t, s)
				if len(r.InitialUsageReservations) != 1 {
					t.Fatalf("Execute before initial reserve: %+v", r.InitialUsageReservations)
				}
				for _, res := range r.InitialUsageReservations {
					if calls == 1 && res.Receipt != nil {
						t.Fatal("initial already settled before Execute")
					}
					if calls > 1 && res.Receipt == nil {
						t.Fatal("round before initial settlement")
					}
				}
				out := map[string]any{"reply": fmt.Sprint(calls)}
				if tc.initialValid || (calls > 1 && tc.roundValid) {
					out["ok"] = true
				}
				result := delegate.Result{Output: out, Tokens: tc.tokens, CostUSD: float64(tc.cost), CostIsSessionTotal: tc.cost > 0, SessionID: "s", SessionFingerprint: "fp", UnreportedCalls: tc.unreported}
				if calls > 1 && tc.fail {
					return result, &delegate.ErrAuthFailed{}
				}
				return result, nil
			})
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, b)
			ex := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			e := New(wf, s, ex)
			rs := e.newRunState("correction", nil)
			rs.ctx = x.context(context.Background())
			output, err := e.executeCorrectionInvocation(rs.ctx, rs, "worker", "actual.scope#loop", wf.Nodes["worker"], nil)
			if calls != tc.wantCalls || (err == nil) != (!tc.fail) {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			before := rs.correctionAccounts.snapshot(&store.Checkpoint{})
			if err == nil {
				if err = e.recordBudget(rs, "worker", output, true); err != nil {
					t.Fatal(err)
				}
			} else {
				e.recordFailedNodeSpend(rs, "worker", output)
			}
			cp := buildCheckpoint(rs, "worker")
			if cp.CorrectionAccounting == nil || cp.BudgetTokensUsed != tc.tokens*tc.wantCalls || cp.BudgetCostUSD != float64(tc.cost) || cp.BudgetIterationsUsed != tc.wantIterations || cp.BudgetUnreportedCalls != tc.unreported*tc.wantCalls || cp.BudgetUnpricedNodes != tc.wantUnpricedNodes {
				t.Fatalf("wrong unique accounting %+v", cp)
			}
			if tc.cost == 0 && cp.BudgetUnpricedTokens != tc.tokens*tc.wantCalls {
				t.Fatalf("unknown tokens %+v", cp)
			}
			if cp.CorrectionAccounting.DailyScopeCostUSD["actual.scope#loop"] != float64(tc.cost) {
				t.Fatalf("actual scope lost %+v", cp.CorrectionAccounting)
			}
			if len(before.IterationMarks) != len(cp.CorrectionAccounting.IterationMarks) {
				t.Fatal("legacy booking changed iteration marks")
			}
			// Lose all volatile state and checkpoint before reconciliation: durable
			// initial + round receipts independently rebuild the one iteration.
			reopened, re := store.New(s.Root())
			if re != nil {
				t.Fatal(re)
			}
			r, re := reopened.LoadRun(context.Background(), rs.runID)
			if re != nil {
				t.Fatal(re)
			}
			fresh := e.newRunState(rs.runID, nil)
			if err = fresh.correctionAccounts.reconcile(r); err != nil {
				t.Fatal(err)
			}
			recovered := buildCheckpoint(fresh, "worker")
			if recovered.BudgetTokensUsed != cp.BudgetTokensUsed || recovered.BudgetCostUSD != cp.BudgetCostUSD || recovered.BudgetIterationsUsed != cp.BudgetIterationsUsed || recovered.BudgetUnpricedNodes != cp.BudgetUnpricedNodes {
				t.Fatalf("reopen lost accounting %+v vs %+v", recovered, cp)
			}
			if err = fresh.correctionAccounts.reconcile(r); err != nil {
				t.Fatal(err)
			}
			again := buildCheckpoint(fresh, "worker")
			if again.BudgetTokensUsed != cp.BudgetTokensUsed || again.BudgetIterationsUsed != cp.BudgetIterationsUsed {
				t.Fatal("reconciliation replay charged twice")
			}
		})
	}
}
func TestObservedInvocationNoDoubleBudgetProjection(t *testing.T) {
	s, x := correctionFixture(t)
	wf := correctionEngineWorkflow()
	wf.Budget.MaxTokens = 100
	calls := 0
	b := observedBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		out := map[string]any{"reply": "x"}
		if calls == 2 {
			out["ok"] = true
		}
		return delegate.Result{Output: out, Tokens: 50, CostUSD: 1}, nil
	})
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, b)
	ex := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	e := New(wf, s, ex)
	rs := e.newRunState("correction", nil)
	rs.ctx = x.context(context.Background())
	_, err := e.executeCorrectionInvocation(rs.ctx, rs, "worker", rs.runID, wf.Nodes["worker"], nil)
	if err != nil || calls != 2 {
		t.Fatalf("settled initial was added twice to guard: calls=%d err=%v", calls, err)
	}
}
func TestObservedInvocationCancellationSettlesOriginal(t *testing.T) {
	s, x := correctionFixture(t)
	wf := correctionEngineWorkflow()
	ctx, cancel := context.WithCancel(x.context(context.Background()))
	calls := 0
	b := observedBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		cancel()
		return delegate.Result{Tokens: 7, UnreportedCalls: 1}, context.Canceled
	})
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, b)
	ex := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	e := New(wf, s, ex)
	rs := e.newRunState("correction", nil)
	rs.ctx = ctx
	_, err := e.executeCorrectionInvocation(ctx, rs, "worker", rs.runID, wf.Nodes["worker"], nil)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal(calls, err)
	}
	r := readCorrection(t, s)
	if len(r.InitialUsageReservations) != 1 || len(r.OutputCorrections) != 0 {
		t.Fatal("cancellation invented correction")
	}
	for _, res := range r.InitialUsageReservations {
		if res.ExecutionEpoch != x.authority.Epoch || res.Receipt == nil || res.Receipt.Usage.Tokens != 7 || res.Receipt.Observation.UnpricedTokens != 7 || !res.Receipt.Observation.CountIteration {
			t.Fatalf("lost paid cancellation %+v", res)
		}
	}
}
func TestObservedAccountingOverflowIsAtomic(t *testing.T) {
	for _, which := range []string{"tokens", "cost", "unpriced", "iterations", "daily"} {
		t.Run(which, func(t *testing.T) {
			b := &SharedBudget{warningsEmitted: map[string]bool{}}
			a, _ := newCorrectionAccounts(b, nil)
			switch which {
			case "tokens":
				b.tokensUsed = int(^uint(0) >> 1)
			case "cost":
				b.costUsed = 1.7976931348623157e308
			case "unpriced":
				b.unpricedTokens = int(^uint(0) >> 1)
			case "iterations":
				b.iterationsUsed = int(^uint(0) >> 1)
			case "daily":
				a.state.DailyScopeCostUSD["scope"] = 1.7976931348623157e308
			}
			initial := store.OutputCorrectionReservation{ID: "initial", InitialUsageID: "initial", ExecutionEpoch: "epoch", ScopeKey: "scope", Receipt: &store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{Tokens: 1}, Observation: &store.UsageObservation{Version: 1, Tokens: 1, UnpricedTokens: 1, CountIteration: true}}}
			if which == "cost" || which == "daily" {
				initial.Receipt.Usage.CostUSD = 1.7976931348623157e308
				initial.Receipt.Observation.OtherCostUSD = initial.Receipt.Usage.CostUSD
			}
			run := &store.Run{InitialUsageReservations: map[string]store.OutputCorrectionReservation{"initial": initial}}
			before := &store.Checkpoint{}
			a.snapshot(before)
			if err := a.reconcile(run); err == nil {
				t.Fatal("overflow accepted")
			}
			after := &store.Checkpoint{}
			a.snapshot(after)
			if before.BudgetTokensUsed != after.BudgetTokensUsed || before.BudgetCostUSD != after.BudgetCostUSD || before.BudgetIterationsUsed != after.BudgetIterationsUsed || len(a.state.UsageMarks) != 0 {
				t.Fatal("overflow partly booked")
			}
		})
	}
}
