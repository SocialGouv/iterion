package mongo

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestUsageV2ProjectionMongoFreezesComponentsAndRecomputesCAS(t *testing.T) {
	s := correctionMongoStore(t)
	ctx, cancel := context.WithTimeout(retryCtx(), 20*time.Second)
	defer cancel()
	r, err := s.CreateRun(ctx, "facts-cas", "workflow", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, mongoExecutionExpected(r), store.ExecutionStart)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	obs := func(value float64) store.OutputCorrectionReceipt {
		component := storetest.UsageV2Delegate(1, "original-model", 1, value, true)
		component.TokenSplit = &store.UsageTokenSplit{Input: 1}
		return storetest.UsageV2Receipt(t, fmt.Sprintf("collection-%g", value), component)
	}
	initial := store.OutputCorrectionReservation{ID: "initial", ExecutionEpoch: a.Epoch, ScopeKey: r.ID, StartedAt: time.Now()}
	if ok, e := s.ReserveInitialUsageIf(ctx, r.ID, a, initial); e != nil || !ok {
		t.Fatal(ok, e)
	}
	if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, store.InitialUsageTarget(initial), obs(10)); e != nil || !ok {
		t.Fatal(ok, e)
	}
	ep := store.OutputCorrectionEpisode{Generation: "old", Revision: 1, Status: "active", Budget: 1}
	if ok, e := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.OutputCorrectionExpected{Absent: true}, ep); e != nil || !ok {
		t.Fatal(ok, e)
	}
	next := ep
	next.Revision++
	next.Attempts = 1
	reservation := store.OutputCorrectionReservation{ID: "paid", InitialUsageID: "initial", ExecutionEpoch: a.Epoch, ScopeKey: r.ID, StartedAt: time.Now()}
	next.Reservations = []store.OutputCorrectionReservation{reservation}
	if ok, e := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.ExpectOutputCorrection(ep), next); e != nil || !ok {
		t.Fatal(ok, e)
	}
	target := reservation.ReceiptTarget("node", "old")
	receipt := obs(12)
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var armed atomic.Bool
	armed.Store(true)
	var updates atomic.Int32
	monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "update" {
			updates.Add(1)
			if armed.CompareAndSwap(true, false) {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		}
	}}
	client, err := mongo.Connect(options.Client().ApplyURI(os.Getenv("ITERION_TEST_MONGO_URI")).SetRegistry(Registry()).SetMonitor(monitor))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(context.Background())
	watched := &Store{runs: client.Database(s.db.Name()).Collection(colRuns)}
	type outcome struct {
		ok  bool
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		ok, e := watched.RecordOutputCorrectionReceipt(ctx, r.ID, target, receipt)
		done <- outcome{ok, e}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first CAS never started")
	}
	if err := s.UpdateRunStatus(ctx, r.ID, store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	retired := fresh.OutputCorrections["node"]
	retired.LedgerKey = "node"
	fresh.OutputCorrectionHistory = append(fresh.OutputCorrectionHistory, retired)
	delete(fresh.OutputCorrections, "node")
	if err := s.SaveRun(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	fresh, err = s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	successor, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, mongoExecutionExpected(fresh), store.ExecutionResume)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	newer := store.OutputCorrectionReservation{ID: "newer", ExecutionEpoch: successor.Epoch, ScopeKey: r.ID, StartedAt: time.Now()}
	if ok, e := s.ReserveInitialUsageIf(ctx, r.ID, successor, newer); e != nil || !ok {
		t.Fatal(ok, e)
	}
	if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, store.InitialUsageTarget(newer), obs(13)); e != nil || !ok {
		t.Fatal(ok, e)
	}
	receipt.Observation.Sessions[0].CostUSD = 120 // Caller reused every nested buffer after CAS reached the wire.
	receipt.Observation.Components[0].CostUSD = 120
	receipt.Observation.Components[0].Route.RequestedModel = "mutated"
	receipt.Observation.Components[0].TokenSplit.Input = 120
	unblock()
	got := <-done
	if got.err != nil || !got.ok {
		t.Fatal(got)
	}
	final, err := s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	old := final.OutputCorrectionHistory[0]
	if updates.Load() != 2 || old.Usage.CostUSD != 0 || old.Usage.Tokens != 1 || old.Status != "active" || final.ExecutionEpoch != successor.Epoch || final.InitialUsageReservations["newer"].Receipt.Usage.CostUSD != 3 {
		t.Fatalf("CAS/facts wrong updates=%d history=%+v new=%+v epoch=%q", updates.Load(), old, final.InitialUsageReservations["newer"], final.ExecutionEpoch)
	}
	if receipt.Usage != (store.OutputCorrectionUsage{}) || old.Reservations[0].Receipt.Observation.Sessions[0].CostUSD != 12 {
		t.Fatal("pure closure mutated captured input")
	}
	stored := old.Reservations[0].Receipt
	if stored.Observation.Components[0].Route.RequestedModel != "original-model" || stored.Observation.Components[0].TokenSplit.Input != 1 || stored.Projection.Lines[0].Route.RequestedModel != "original-model" || stored.Projection.Lines[1].PreviousMaxUSD != 13 || stored.Projection.Lines[1].CostUSD != 0 || stored.Projection.Lines[1].Route != nil {
		t.Fatalf("CAS projection lost original facts or used stale marks: %+v", stored)
	}
	if _, e := store.FreezeSettledUsageReceipt(*stored); e != nil {
		t.Fatal(e)
	}
	t.Logf("updates=%d stale_receipt_cost=0 original_route_and_split_preserved=true previous_max=13 newer_cost=3 active_epoch_preserved=true", updates.Load())
}
