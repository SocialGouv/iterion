package runtime

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

func TestUsageV2InitialAndTwoRoundsOwnOneCollector(t *testing.T) {
	s, x := correctionFixture(t)
	wf := correctionEngineWorkflow()
	calls := 0
	backend := observedBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		output := map[string]any{"reply": fmt.Sprint(calls)}
		if calls == 3 {
			output["ok"] = true
		}
		return delegate.Result{Output: output, Tokens: 1, CostUSD: 1}, nil
	})
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, backend)
	executor := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	engine := New(wf, s, executor, WithOutputCorrectionBudget(2))
	rs := engine.newRunState("correction", nil)
	ctx := x.context(context.Background())
	rs.ctx = ctx
	if _, err := engine.executeCorrectionInvocation(ctx, rs, "worker", rs.runID, wf.Nodes["worker"], nil); err != nil {
		t.Fatal(err)
	}
	r := readCorrection(t, s)
	if calls != 3 || len(r.InitialUsageReservations) != 1 {
		t.Fatalf("calls=%d initials=%d", calls, len(r.InitialUsageReservations))
	}
	var initialID, collectionID string
	for id, res := range r.InitialUsageReservations {
		initialID = id
		collectionID = res.Receipt.Observation.CollectionID
		if collectionID == "" || res.Receipt.Projection.CollectionID != collectionID {
			t.Fatal("missing stable collector identity")
		}
		if res.Receipt.Observation.Components[0].Ordinal != 1 {
			t.Fatal("initial ordinal")
		}
	}
	rounds := r.OutputCorrections["worker"].Reservations
	if len(rounds) != 2 {
		t.Fatalf("rounds=%+v", rounds)
	}
	for i, res := range rounds {
		if res.Receipt.Observation.CollectionID != collectionID || res.Receipt.Projection.CollectionID != collectionID || res.InitialUsageID != initialID || len(res.Receipt.Observation.Components) != 1 || res.Receipt.Observation.Components[0].Ordinal != uint64(i+2) {
			t.Fatalf("round%d %+v", i, res)
		}
	}
	accounts, err := newCorrectionAccounts(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.reconcile(r); err != nil {
		t.Fatal(err)
	}
	before := accounts.state
	for id, res := range r.InitialUsageReservations {
		res.Receipt.Projection.Lines[0].Route.RequestedModel = "forged-after-persist"
		r.InitialUsageReservations[id] = res
	}
	if err := accounts.reconcile(r); err == nil {
		t.Fatal("runtime accepted malformed v2 projection")
	}
	if !reflect.DeepEqual(before, accounts.state) {
		t.Fatal("invalid projection partially altered runtime marks")
	}
	t.Log("real Engine+ClawExecutor: one InitialUsageID, initial ordinal1, round1 ordinal2, round2 ordinal3; same collector enforced by owned handle")
}
