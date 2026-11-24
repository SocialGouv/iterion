package storetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type observationStore interface {
	store.RunStore
	store.OutputCorrectionCASStore
	ReserveInitialUsageIf(context.Context, string, store.RunExecutionAuthority, store.OutputCorrectionReservation) (bool, error)
}

func RunUsageObservationPrototype(t *testing.T, makeStore func(*testing.T) store.RunStore) {
	for _, other := range []bool{false, true} {
		t.Run(fmt.Sprintf("other=%v", other), func(t *testing.T) {
			ctx := store.WithTenant(context.Background(), "observation-prototype")
			raw := makeStore(t)
			s := raw.(observationStore)
			r, err := s.CreateRun(ctx, "observations", "workflow", nil)
			if err != nil {
				t.Fatal(err)
			}
			expected := func(r *store.Run) store.RunExecutionExpectation {
				return store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}
			}
			a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, expected(r), store.ExecutionStart)
			if err != nil || !ok {
				t.Fatalf("acquire %v %v", ok, err)
			}
			load := func() *store.Run {
				r, e := s.LoadRun(ctx, "observations")
				if e != nil {
					t.Fatal(e)
				}
				return r
			}
			reserve := func(id string, a store.RunExecutionAuthority) store.OutputCorrectionReceiptTarget {
				res := store.OutputCorrectionReservation{ID: id, ExecutionEpoch: a.Epoch, ScopeKey: "observations", StartedAt: time.Now()}
				ok, e := s.ReserveInitialUsageIf(ctx, "observations", a, res)
				if e != nil || !ok {
					t.Fatalf("reserve %v %v", ok, e)
				}
				return store.InitialUsageTarget(res)
			}
			observation := func(tokens int, session, extra float64) store.OutputCorrectionReceipt {
				return store.OutputCorrectionReceipt{Observation: &store.UsageObservation{Version: 1, Tokens: tokens, OtherCostUSD: extra, Sessions: []store.SessionCostObservation{{Backend: "claude_code", SessionID: "same", Fingerprint: "cred", CostUSD: session}}}}
			}
			first := reserve("first", a)
			if ok, e := s.RecordOutputCorrectionReceipt(ctx, "observations", first, observation(5, 10, 0)); e != nil || !ok {
				t.Fatalf("first %v %v", ok, e)
			}
			r = load()
			if len(r.OutputCorrections) != 0 || r.InitialUsageReservations["first"].Receipt.Usage.CostUSD != 10 {
				t.Fatalf("initial incorrectly creates episode: %+v", r.InitialUsageReservations)
			}
			ep := store.OutputCorrectionEpisode{Generation: "generation", Revision: 1, Status: "active", Budget: 2}
			if ok, e := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.OutputCorrectionExpected{Absent: true}, ep); e != nil || !ok {
				t.Fatal(ok, e)
			}
			for _, id := range []string{"round1", "late"} {
				old := load().OutputCorrections["node"]
				next := old
				next.Revision++
				next.Attempts++
				next.Reservations = append(append([]store.OutputCorrectionReservation(nil), old.Reservations...), store.OutputCorrectionReservation{ID: id, InitialUsageID: "first", ExecutionEpoch: a.Epoch, ScopeKey: "observations", StartedAt: time.Now()})
				if ok, e := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.ExpectOutputCorrection(old), next); e != nil || !ok {
					t.Fatal(ok, e)
				}
			}
			ep = load().OutputCorrections["node"]
			round := ep.Reservations[0].ReceiptTarget("node", ep.Generation)
			late := ep.Reservations[1].ReceiptTarget("node", ep.Generation)
			reading, extra := 12.0, 0.0
			if other {
				reading, extra = 10, 2
			}
			roundBody := observation(5, reading, extra)
			if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, round, roundBody); e != nil || !ok {
				t.Fatal(ok, e)
			}
			// Simulate loss of every volatile runtime object: only the next store read
			// supplies the session maximum. A fresh initial observation now differs.
			if fs, ok := s.(*store.FilesystemRunStore); ok {
				reopened, e := store.New(fs.Root())
				if e != nil {
					t.Fatal(e)
				}
				s = reopened
			}
			second := reserve("second", a)
			if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, second, observation(7, 12, 0)); e != nil || !ok {
				t.Fatal(ok, e)
			}
			r = load()
			want := 0.0
			if other {
				want = 2
			}
			if got := r.InitialUsageReservations["second"].Receipt.Usage.CostUSD; got != want {
				t.Fatalf("crash/reopen basis cost=%g want=%g", got, want)
			}
			if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, round, roundBody); e != nil || ok {
				t.Fatalf("duplicate after maxima changed %v %v", ok, e)
			}
			before := load()
			different := observation(6, reading, extra)
			if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, round, different); ok || !errors.Is(e, store.ErrInvalidOutputCorrection) {
				t.Fatalf("contradiction %v %v", ok, e)
			}
			if !reflect.DeepEqual(before, load()) {
				t.Fatal("contradiction mutated run")
			}
			if err := s.UpdateRunStatus(ctx, r.ID, store.RunStatusPausedWaitingHuman, ""); err != nil {
				t.Fatal(err)
			}
			r = load()
			retired := r.OutputCorrections["node"]
			retired.LedgerKey = "node"
			r.OutputCorrectionHistory = append(r.OutputCorrectionHistory, retired)
			delete(r.OutputCorrections, "node")
			if err := s.SaveRun(ctx, r); err != nil {
				t.Fatal(err)
			}
			r = load()
			newGrant, ok, e := s.AcquireRunExecutionIf(ctx, r.ID, expected(r), store.ExecutionResume)
			if e != nil || !ok {
				t.Fatal(ok, e)
			}
			if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, late, observation(3, 13, 0)); e != nil || !ok {
				t.Fatal(ok, e)
			}
			r = load()
			if r.ExecutionEpoch != newGrant.Epoch || r.OutputCorrectionHistory[0].Status != "active" || r.OutputCorrectionHistory[0].Attempts != 2 || r.OutputCorrectionHistory[0].Reservations[1].Receipt.Usage.CostUSD != 1 {
				t.Fatal("late receipt renewed owner or changed verdict")
			}
			forged := round
			forged.ExecutionEpoch = newGrant.Epoch
			if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, forged, roundBody); ok || !errors.Is(e, store.ErrInvalidOutputCorrection) {
				t.Fatal("identity after duplicate", ok, e)
			}
			targets := []store.OutputCorrectionReceiptTarget{reserve("parallel1", newGrant), reserve("parallel2", newGrant)}
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			for i := range targets {
				wg.Go(func() {
					ok, e := s.RecordOutputCorrectionReceipt(ctx, "observations", targets[i], observation(1, float64(14+i), 0))
					if !ok || e != nil {
						errs <- fmt.Errorf("parallel settlement %v %w", ok, e)
					}
				})
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				t.Fatal(e)
			}
			r = load()
			total, tokens := 0.0, 0
			for _, res := range r.InitialUsageReservations {
				if res.Receipt != nil {
					total += res.Receipt.Usage.CostUSD
					tokens += res.Receipt.Usage.Tokens
				}
			}
			for _, ep := range r.OutputCorrectionHistory {
				total += ep.Usage.CostUSD
				tokens += ep.Usage.Tokens
			}
			wantTotal := 15.0
			if other {
				wantTotal += 2
			}
			if total != wantTotal || tokens != 22 {
				t.Fatalf("totals cost=%g/%g tokens=%d/22", total, wantTotal, tokens)
			}
			for _, v := range r.UsageSessionMarks {
				if v != 15 {
					t.Fatalf("maximum=%g", v)
				}
			}

			badInitial := first
			badInitial.ExecutionEpoch = newGrant.Epoch
			if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, badInitial, observation(5, 10, 0)); ok || !errors.Is(e, store.ErrInvalidOutputCorrection) {
				t.Fatal("initial duplicate stole successor epoch", ok, e)
			}
			if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, first, observation(6, 10, 0)); ok || !errors.Is(e, store.ErrInvalidOutputCorrection) {
				t.Fatal("initial duplicate changed body", ok, e)
			}
			stale := store.OutputCorrectionReservation{ID: "stale", ExecutionEpoch: a.Epoch, ScopeKey: r.ID, StartedAt: time.Now()}
			if ok, e := s.ReserveInitialUsageIf(ctx, r.ID, a, stale); ok || e != nil {
				t.Fatal("stale authority reserved initial call", ok, e)
			}
			untouched := load()
			malformed := observation(1, 16, 0)
			malformed.Observation.Sessions = append(malformed.Observation.Sessions, malformed.Observation.Sessions[0])
			if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, targets[0], malformed); ok || !errors.Is(e, store.ErrInvalidOutputCorrection) {
				t.Fatal("duplicate session basis accepted", ok, e)
			}
			if !reflect.DeepEqual(untouched, load()) {
				t.Fatal("rejected observation mutated accounting")
			}
			if other {
				t.Logf("additive world: next delta=2 total=17 tokens=22 session_max=15")
			} else {
				t.Logf("session world: next delta=0 total=15 tokens=22 session_max=15")
			}
		})
	}
}

func RunUsageObservationPricingPrototype(t *testing.T, makeStore func(*testing.T) store.RunStore) {
	ctx := store.WithTenant(context.Background(), "observation-pricing")
	s := makeStore(t).(observationStore)
	r, e := s.CreateRun(ctx, "pricing", "workflow", nil)
	if e != nil {
		t.Fatal(e)
	}
	a, ok, e := s.AcquireRunExecutionIf(ctx, r.ID, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	for i, unpriced := range []int{0, 0, 4} {
		id := fmt.Sprint(i)
		res := store.OutputCorrectionReservation{ID: id, ExecutionEpoch: a.Epoch, ScopeKey: r.ID, StartedAt: time.Now()}
		if ok, e := s.ReserveInitialUsageIf(ctx, r.ID, a, res); e != nil || !ok {
			t.Fatal(ok, e)
		}
		receipt := store.OutputCorrectionReceipt{Observation: &store.UsageObservation{Version: 1, Tokens: 4, UnpricedTokens: unpriced, Sessions: []store.SessionCostObservation{{Backend: "claude_code", SessionID: "same", Fingerprint: "cred", CostUSD: 10}}}}
		if ok, e := s.RecordOutputCorrectionReceipt(ctx, r.ID, store.InitialUsageTarget(res), receipt); e != nil || !ok {
			t.Fatal(ok, e)
		}
	}
	r, e = s.LoadRun(ctx, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	known, unknown := r.InitialUsageReservations["1"].Receipt, r.InitialUsageReservations["2"].Receipt
	if known.Usage.CostUSD != 0 || unknown.Usage.CostUSD != 0 || known.Observation.UnpricedTokens != 0 || unknown.Observation.UnpricedTokens != 4 {
		t.Fatalf("marginal cost erased price visibility: known=%+v unknown=%+v", known, unknown)
	}
	t.Log("both marginal costs=0; explicit unpriced tokens distinguish already-paid=0 from unknown=4")
}
