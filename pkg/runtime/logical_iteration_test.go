package runtime

import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
	"time"
)

func logicalIterationReceipt(id, logical string) store.OutputCorrectionReservation {
	return store.OutputCorrectionReservation{ID: id, InitialUsageID: id, LogicalInvocationID: logical, ExecutionEpoch: "epoch", ScopeKey: "scope", Receipt: &store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{Tokens: 1}, Observation: &store.UsageObservation{Version: 1, Tokens: 1, UnpricedTokens: 1, CountIteration: true}}}
}
func TestLogicalIterationRestart(t *testing.T) {
	for _, codec := range []string{"json", "bson"} {
		t.Run(codec, func(t *testing.T) {
			wf := validationWorkflow()
			wf.Loops = map[string]*ir.Loop{"loop": {Name: "loop", Body: map[string]bool{"my_agent": true}}}
			eng := &Engine{workflow: wf}
			identity := func(loop int, branch string) string {
				_, id := eng.correctionInvocationIdentity(&runState{loopCounters: map[string]int{"loop": loop}, branchLocal: branch != "", correctionScope: branch}, "my_agent")
				return id
			}
			b := &SharedBudget{}
			a, err := newCorrectionAccounts(b, nil)
			if err != nil {
				t.Fatal(err)
			}
			row := &store.Run{ID: "run", InitialUsageReservations: map[string]store.OutputCorrectionReservation{"a": logicalIterationReceipt("a", identity(0, "")), "retry": logicalIterationReceipt("retry", identity(0, ""))}}
			if err = a.reconcile(row); err != nil {
				t.Fatal(err)
			}
			if b.iterationsUsed != 1 {
				t.Fatalf("retry=%d", b.iterationsUsed)
			}
			cp := &store.Checkpoint{}
			saved := a.snapshot(cp)
			var wire []byte
			var restored store.CorrectionAccounting
			if codec == "json" {
				wire, err = json.Marshal(saved)
				if err == nil {
					err = json.Unmarshal(wire, &restored)
				}
			} else {
				wire, err = bson.Marshal(saved)
				if err == nil {
					err = bson.Unmarshal(wire, &restored)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			b = &SharedBudget{}
			b.Restore(cp.BudgetTokensUsed, cp.BudgetCostUSD, cp.BudgetIterationsUsed, 0, cp.BudgetUnpricedTokens, cp.BudgetUnpricedNodes, cp.BudgetUnreportedCalls)
			a, err = newCorrectionAccounts(b, &restored)
			if err != nil {
				t.Fatal(err)
			}
			if err = a.reconcile(row); err != nil {
				t.Fatal(err)
			}
			if b.iterationsUsed != 1 {
				t.Fatalf("replay=%d", b.iterationsUsed)
			}
			for id, logical := range map[string]string{"next": identity(1, ""), "branch-a": identity(1, "a"), "branch-b": identity(1, "b")} {
				row.InitialUsageReservations[id] = logicalIterationReceipt(id, logical)
			}
			if err = a.reconcile(row); err != nil {
				t.Fatal(err)
			}
			if b.iterationsUsed != 4 {
				t.Fatalf("loop/fanout=%d", b.iterationsUsed)
			}
			child := &store.Run{ID: "child", InitialUsageReservations: map[string]store.OutputCorrectionReservation{"child-a": logicalIterationReceipt("child-a", identity(0, ""))}}
			if err = a.reconcile(child); err != nil {
				t.Fatal(err)
			}
			if b.iterationsUsed != 5 {
				t.Fatalf("child=%d", b.iterationsUsed)
			}
		})
	}
}
func TestLogicalIterationReservationFSReopen(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	r, err := s.CreateRun(ctx, "r", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	auth, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	res := store.OutputCorrectionReservation{ID: "initial", ExecutionEpoch: auth.Epoch, ScopeKey: "scope", StartedAt: time.Now(), LogicalInvocationID: "node=a;branch=b;loops=l=1"}
	if ok, err = s.ReserveInitialUsageIf(ctx, r.ID, auth, res); err != nil || !ok {
		t.Fatal(ok, err)
	}
	s, err = store.New(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.InitialUsageReservations[res.ID].LogicalInvocationID != res.LogicalInvocationID {
		t.Fatal("lost logical identity")
	}
	if ok, err = s.ReserveInitialUsageIf(ctx, r.ID, auth, res); err != nil || ok {
		t.Fatalf("idempotency %v %v", ok, err)
	}
	other := res
	other.LogicalInvocationID = "other"
	if _, err = s.ReserveInitialUsageIf(ctx, r.ID, auth, other); err == nil {
		t.Fatal("contradictory identity accepted")
	}
	other = res
	other.ID = "bad"
	other.LogicalInvocationID = string([]byte{255})
	if _, err = s.ReserveInitialUsageIf(ctx, r.ID, auth, other); err == nil {
		t.Fatal("bad utf8 accepted")
	}
	for _, codec := range []string{"json", "bson"} {
		var wire []byte
		var restored store.OutputCorrectionReservation
		if codec == "json" {
			wire, err = json.Marshal(res)
			if err == nil {
				err = json.Unmarshal(wire, &restored)
			}
		} else {
			wire, err = bson.Marshal(res)
			if err == nil {
				err = bson.Unmarshal(wire, &restored)
			}
		}
		if err != nil || restored.LogicalInvocationID != res.LogicalInvocationID {
			t.Fatal(codec, err, restored)
		}
	}
}
