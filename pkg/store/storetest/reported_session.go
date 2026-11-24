package storetest

import (
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"math"
	"reflect"
	"testing"
	"time"
)

// RunReportedSession checks the shared FS/Mongo receipt algebra, including
// delayed lower totals and identity separation. It makes no fork-origin claim.
func RunReportedSession(t *testing.T, makeStore func(*testing.T) store.RunStore) {
	s := makeStore(t).(observationStore)
	ctx := store.WithTenant(t.Context(), "reported-session")
	r, err := s.CreateRun(ctx, "reported-session", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.TenantID = "reported-session"
	if err = s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	total := 0.0
	for i, tc := range []struct {
		amount      float64
		fingerprint string
		want        float64
	}{
		{1, "a", 1}, {1.6, "a", .6}, {1.2, "a", 0}, {1.6, "a", 0}, {1.6, "b", 1.6},
	} {
		id := fmt.Sprint("r", i)
		res := store.OutputCorrectionReservation{ID: id, ExecutionEpoch: a.Epoch, ScopeKey: r.ID, StartedAt: time.Now()}
		if ok, err = s.ReserveInitialUsageIf(ctx, r.ID, a, res); err != nil || !ok {
			t.Fatal(ok, err)
		}
		c := store.UsageComponent{Ordinal: 1, Source: store.UsageGenericReported, ReportedSession: &store.ReportedEconomicSession{Backend: "declared", SessionID: "same", Fingerprint: tc.fingerprint}, Tokens: 2, CostKnown: true, CostKind: store.UsageSessionTotal, CostUSD: tc.amount}
		receipt := UsageV2Receipt(t, id, c)
		target := store.InitialUsageTarget(res)
		if ok, err = s.RecordOutputCorrectionReceipt(ctx, r.ID, target, receipt); err != nil || !ok {
			t.Fatal(ok, err)
		}
		row, e := s.LoadRun(ctx, r.ID)
		if e != nil {
			t.Fatal(e)
		}
		got := row.InitialUsageReservations[id].Receipt
		if math.Abs(got.Usage.CostUSD-tc.want) > 1e-12 || got.Usage.Tokens != 2 {
			t.Fatalf("reading %d: %+v", i, got.Usage)
		}
		if got.Observation.Version != 2 || got.Observation.Components[0].ReportedSession == nil || got.Projection == nil || len(got.Projection.Lines) != 2 || got.Projection.Lines[0].Source != store.UsageGenericReported || got.Projection.Lines[0].Route != nil || got.Projection.Lines[1].Route != nil {
			t.Fatalf("lost declared provenance: %+v", got)
		}
		if _, err = store.FreezeSettledUsageReceipt(*got); err != nil {
			t.Fatal(err)
		}
		if ok, err = s.RecordOutputCorrectionReceipt(ctx, r.ID, target, receipt); err != nil || ok {
			t.Fatal("replay", ok, err)
		}
		after, e := s.LoadRun(ctx, r.ID)
		if e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(row, after) {
			t.Fatal("replay changed state")
		}
		total += got.Usage.CostUSD
	}
	if math.Abs(total-3.2) > 1e-12 {
		t.Fatal(total)
	}
}
