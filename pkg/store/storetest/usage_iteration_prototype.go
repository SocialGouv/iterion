package storetest

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"reflect"
	"testing"
	"time"
)

func RunUsageIterationPrototype(t *testing.T, makeStore func(*testing.T) store.RunStore) {
	ctx := store.WithTenant(context.Background(), "observation-iteration")
	s := makeStore(t).(observationStore)
	r, err := s.CreateRun(ctx, "iteration", "workflow", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	initial := store.OutputCorrectionReservation{ID: "initial", InitialUsageID: "initial", ExecutionEpoch: a.Epoch, ScopeKey: "scope#branch", StartedAt: time.Now()}
	if ok, err := s.ReserveInitialUsageIf(ctx, r.ID, a, initial); err != nil || !ok {
		t.Fatal(ok, err)
	}
	zero := store.OutputCorrectionReceipt{Observation: &store.UsageObservation{Version: 1}}
	if ok, err := s.RecordOutputCorrectionReceipt(ctx, r.ID, store.InitialUsageTarget(initial), zero); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ep := store.OutputCorrectionEpisode{Generation: "generation", Revision: 1, Status: "active", Budget: 1}
	if ok, err := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.OutputCorrectionExpected{Absent: true}, ep); err != nil || !ok {
		t.Fatal(ok, err)
	}
	old, err := s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	ep = old.OutputCorrections["node"]
	next := ep
	next.Revision++
	next.Attempts++
	res := store.OutputCorrectionReservation{ID: "round", InitialUsageID: "initial", ExecutionEpoch: a.Epoch, ScopeKey: initial.ScopeKey, StartedAt: time.Now()}
	next.Reservations = []store.OutputCorrectionReservation{res}
	if ok, err := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.ExpectOutputCorrection(ep), next); err != nil || !ok {
		t.Fatal(ok, err)
	}
	target := res.ReceiptTarget("node", ep.Generation)
	success := store.OutputCorrectionReceipt{Observation: &store.UsageObservation{Version: 1, CountIteration: true}}
	if ok, err := s.RecordOutputCorrectionReceipt(ctx, r.ID, target, success); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if fs, ok := s.(*store.FilesystemRunStore); ok {
		s, err = store.New(fs.Root())
		if err != nil {
			t.Fatal(err)
		}
	}
	final, err := s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.InitialUsageReservations["initial"].Receipt.Observation.CountIteration || !final.OutputCorrections["node"].Reservations[0].Receipt.Observation.CountIteration {
		t.Fatal("zero success fact lost at reopen")
	}
	if final.ExecutionEpoch != a.Epoch || final.OutputCorrections["node"].Status != "active" {
		t.Fatal("iteration fact granted authority or verdict")
	}
	if ok, err := s.RecordOutputCorrectionReceipt(ctx, r.ID, target, success); err != nil || ok {
		t.Fatal("identical iteration fact not idempotent", ok, err)
	}
	if ok, err := s.RecordOutputCorrectionReceipt(ctx, r.ID, target, zero); ok || !errors.Is(err, store.ErrInvalidOutputCorrection) {
		t.Fatal("changed iteration fact did not conflict", ok, err)
	}
	after, err := s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(final, after) {
		t.Fatal("conflicting count changed run")
	}
	t.Log("zero initial=false; zero round=true; replay no-op; body conflict; epoch/status unchanged")
}
