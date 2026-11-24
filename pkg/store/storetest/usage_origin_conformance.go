package storetest

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"reflect"
	"testing"
	"time"
)

func RunUsageOriginConformance(t *testing.T, makeStore func(*testing.T) store.RunStore) {
	ctx := store.WithTenant(context.Background(), "origin-team")
	s := makeStore(t).(observationStore)
	r, err := s.CreateRun(ctx, "origin-run", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.TenantID = "origin-team"
	if err = s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected := func(r *store.Run) store.RunExecutionExpectation {
		return store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}
	}
	a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, expected(r), store.ExecutionStart)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	load := func() *store.Run {
		v, e := s.LoadRun(ctx, r.ID)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	origin := store.UsageOrigin{Version: 1, RunID: r.ID, Publication: "2026-11-04T02:00:00+02:00", TenantID: "origin-team", OrgID: "origin-org"}
	canonical, err := store.CanonicalUsageOrigin(origin)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("initial%d", i)
		res := store.OutputCorrectionReservation{ID: id, InitialUsageID: id, ExecutionEpoch: a.Epoch, ScopeKey: fmt.Sprintf("scope%d", i), StartedAt: time.Now()}
		supplied := origin
		if i < 2 {
			res.Origin = &supplied
		}
		if ok, err = s.ReserveInitialUsageIf(ctx, r.ID, a, res); err != nil || !ok {
			t.Fatal(ok, err)
		}
		supplied.OrgID = "mutated-after-reserve"
		if i < 2 {
			bad := res
			wrong := canonical
			wrong.PoolGrantless = true
			bad.Origin = &wrong
			if ok, err = s.ReserveInitialUsageIf(ctx, r.ID, a, bad); ok || !errors.Is(err, store.ErrInvalidOutputCorrection) {
				t.Fatalf("origin overwrite %v %v", ok, err)
			}
		}
		obs := store.OutputCorrectionReceipt{Observation: &store.UsageObservation{Version: 1, Tokens: 2, OtherCostUSD: 1, Sessions: []store.SessionCostObservation{{Backend: "b", SessionID: id, CostUSD: 1}}}}
		if ok, err = s.RecordOutputCorrectionReceipt(ctx, r.ID, store.InitialUsageTarget(res), obs); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if i >= 2 {
			continue
		}
		key := fmt.Sprintf("node%d", i)
		ep := store.OutputCorrectionEpisode{Generation: fmt.Sprintf("g%d", i), Revision: 1, Status: "active", Budget: 1}
		if ok, err = s.SetRunOutputCorrectionIf(ctx, r.ID, a, key, store.OutputCorrectionExpected{Absent: true}, ep); err != nil || !ok {
			t.Fatal(ok, err)
		}
		old := load().OutputCorrections[key]
		next := old
		next.Revision++
		next.Attempts = 1
		round := store.OutputCorrectionReservation{ID: "round", InitialUsageID: id, ExecutionEpoch: a.Epoch, ScopeKey: res.ScopeKey, StartedAt: time.Now()}
		next.Reservations = []store.OutputCorrectionReservation{round}
		bad := next
		bad.Reservations = append([]store.OutputCorrectionReservation(nil), next.Reservations...)
		bad.Reservations[0].Origin = &canonical
		if ok, err = s.SetRunOutputCorrectionIf(ctx, r.ID, a, key, store.ExpectOutputCorrection(old), bad); ok || !errors.Is(err, store.ErrInvalidOutputCorrection) {
			t.Fatalf("round origin redefinition %v %v", ok, err)
		}
		if ok, err = s.SetRunOutputCorrectionIf(ctx, r.ID, a, key, store.ExpectOutputCorrection(old), next); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if ok, err = s.RecordOutputCorrectionReceipt(ctx, r.ID, round.ReceiptTarget(key, ep.Generation), store.OutputCorrectionReceipt{Observation: &store.UsageObservation{Version: 1, Tokens: 3, OtherCostUSD: 2}}); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	if err = s.UpdateRunStatus(ctx, r.ID, store.RunStatusPausedOperator, ""); err != nil {
		t.Fatal(err)
	}
	fresh := load()
	retired := fresh.OutputCorrections["node0"]
	fresh.OutputCorrectionHistory = append(fresh.OutputCorrectionHistory, retired)
	delete(fresh.OutputCorrections, "node0")
	if err = s.SaveRun(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	fresh = load()
	successor, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, expected(fresh), store.ExecutionResume)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if fs, ok := s.(*store.FilesystemRunStore); ok {
		reopened, e := store.New(fs.Root())
		if e != nil {
			t.Fatal(e)
		}
		s = reopened
	}
	before := load()
	items, err := store.ReadReceiptsForOrigin(ctx, s, origin)
	if err != nil || len(items) != 4 {
		t.Fatalf("origin receipts got=%+v err=%v", items, err)
	}
	total := float64(0)
	tokens := 0
	targets := map[store.OutputCorrectionReceiptTarget]bool{}
	for _, item := range items {
		if item.Origin != canonical || item.Target.ExecutionEpoch != a.Epoch || item.Target.ExecutionEpoch == successor.Epoch || targets[item.Target] {
			t.Fatalf("retargeted/duplicate receipt %+v", item)
		}
		targets[item.Target] = true
		total += item.Usage.CostUSD
		tokens += item.Usage.Tokens
	}
	if total != 8 || tokens != 10 {
		t.Fatalf("origin sum cost=%g tokens=%d", total, tokens)
	}
	if !reflect.DeepEqual(before, load()) {
		t.Fatal("read created authority or changed run")
	}
	for _, tc := range []struct {
		name    string
		change  func(*store.UsageOrigin)
		wantErr bool
	}{
		{"next-publication", func(o *store.UsageOrigin) { o.Publication = "2026-11-04T01:00:00Z" }, false},
		{"same-identity-other-org", func(o *store.UsageOrigin) { o.OrgID = "other" }, true},
		{"same-identity-grantless", func(o *store.UsageOrigin) { o.PoolGrantless = true }, true},
		{"other-tenant", func(o *store.UsageOrigin) { o.TenantID = "other" }, true},
		{"invalid-stamp", func(o *store.UsageOrigin) { o.Publication = "bad" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := origin
			tc.change(&other)
			got, e := store.ReadReceiptsForOrigin(ctx, s, other)
			if tc.wantErr && e == nil || !tc.wantErr && (e != nil || len(got) != 0) {
				t.Fatalf("selection %d %v", len(got), e)
			}
		})
	}
	// The selector copies, even when it operates directly on an in-memory row.
	selected, err := store.ReceiptsForOrigin(before, origin)
	if err != nil {
		t.Fatal(err)
	}
	for i := range selected {
		if len(selected[i].Observation.Sessions) > 0 {
			selected[i].Observation.Sessions[0].CostUSD = 999
		}
	}
	after, err := store.ReceiptsForOrigin(before, origin)
	if err != nil || !reflect.DeepEqual(items, after) {
		t.Fatal("projection aliases durable facts", err)
	}
	// Current+history duplicates are idempotent only for the exact same body.
	duplicate := *before
	duplicate.OutputCorrectionHistory = append(append([]store.OutputCorrectionEpisode(nil), before.OutputCorrectionHistory...), before.OutputCorrections["node1"])
	twice, err := store.ReceiptsForOrigin(&duplicate, origin)
	if err != nil || !reflect.DeepEqual(twice, items) {
		t.Fatal("duplicate target double counted", err)
	}
	last := len(duplicate.OutputCorrectionHistory) - 1
	ep := duplicate.OutputCorrectionHistory[last]
	ep.Reservations = append([]store.OutputCorrectionReservation(nil), ep.Reservations...)
	rec := *ep.Reservations[0].Receipt
	rec.Usage.CostUSD++
	ep.Reservations[0].Receipt = &rec
	duplicate.OutputCorrectionHistory[last] = ep
	if _, err = store.ReceiptsForOrigin(&duplicate, origin); !errors.Is(err, store.ErrInvalidOutputCorrection) {
		t.Fatalf("contradictory duplicate accepted %v", err)
	}
	t.Log("origin=current+history targets=4 tokens=10 cost=8; local/legacy=excluded successor=unchanged")
}
