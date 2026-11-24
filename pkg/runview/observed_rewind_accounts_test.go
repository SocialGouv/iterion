package runview

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

type observedRewindBackend struct{ delegate.Backend }

func (b *observedRewindBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	cost := 0.
	if task.NodeID == "branch_a" {
		cost = 1
	}
	return delegate.Result{Output: map[string]any{"value": "ok"}, CostUSD: cost}, nil
}
func TestObservedDailyScopeSurvivesServiceRewind(t *testing.T) {
	const runID = "run-rewind-subject"
	scope := runID + "#branch_split_branch_a#root"
	cp := &store.Checkpoint{NodeID: "merge", Outputs: outputsOf("survey", "branch_a", "branch_b"), BudgetCostUSD: 3, BudgetIterationsUsed: 1, CorrectionAccounting: &store.CorrectionAccounting{Version: 1, DailyScopeCostUSD: map[string]float64{scope: 3}}, Parallel: &store.ParallelCheckpoint{RouterNodeID: "split", InvocationKey: "split@root", Branches: map[string]*store.BranchCheckpoint{"branch_split_branch_a": {BranchID: "branch_split_branch_a", StartNodeID: "branch_a", CostUSD: 3}}}}
	svc, st, id := seedRun(t, fanOutBot, cp, store.RunStatusFailedResumable)
	guard := runtime.NewDailyCapGuard(store.AsSpendStore(st), nil, runtime.DailyCapConfig{MaxCostPerDayUSD: 100})
	if _, err := guard.Record(context.Background(), scope, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Rewind(context.Background(), RewindSpec{RunID: id, NodeID: "branch_a", KeepFiles: true, Force: true}); err != nil {
		t.Fatal(err)
	}
	row, err := st.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Checkpoint.Parallel != nil || row.Checkpoint.CorrectionAccounting == nil || row.Checkpoint.CorrectionAccounting.DailyScopeCostUSD[scope] != 3 {
		t.Fatalf("rewind removed durable scope %+v", row.Checkpoint)
	}
	wf := ir.Compile(parsertest.Parse("main.bot", fanOutBot).File).Workflow
	for _, n := range wf.Nodes {
		if a, ok := n.(*ir.AgentNode); ok {
			a.Backend = delegate.BackendClaudeCode
			a.Readonly = true
		}
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, &observedRewindBackend{})
	ex := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg))
	eng := runtime.New(wf, st, ex, runtime.WithWorkDir(t.TempDir()), runtime.WithDailyCap(guard))
	if err = eng.Resume(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	day, err := store.AsSpendStore(st).LoadDailySpend(context.Background(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	if day.SpentUSD != 4 || day.RunsContributed[scope] != 4 {
		t.Fatalf("rewind refunded branch daily spend %+v", day)
	}
	row, err = st.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Checkpoint == nil || row.Checkpoint.BudgetCostUSD != 4 {
		t.Fatalf("rewind refunded budget %+v", row.Checkpoint)
	}
	t.Logf("real Service.Rewind cleared Parallel; resumed branch daily $3+$1=$%g", day.SpentUSD)
}
