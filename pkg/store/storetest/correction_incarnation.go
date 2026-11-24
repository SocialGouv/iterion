package storetest

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

func runCorrectionIncarnationConformance(t *testing.T, factory Factory) {
	t.Run("ChildBirthIdentity", func(t *testing.T) {
		s := factory(t)
		if _, err := s.CreateRun(testCtx(), "parent", "wf", nil); err != nil {
			t.Fatal(err)
		}
		creator := store.AsParentedRunCreator(s)
		if creator == nil {
			t.Fatal("missing parented creation")
		}
		a, err := creator.CreateChildRun(testCtx(), "child-birth", "wf", "parent", nil)
		if err != nil {
			t.Fatal(err)
		}
		if a.ExecutionEpoch == "" {
			t.Fatal("child birth has no epoch")
		}
		if err := s.DeleteRun(testCtx(), a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PruneDeletionMarkers(testCtx(), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		b, err := creator.CreateChildRun(testCtx(), a.ID, "wf", "parent", nil)
		if err != nil {
			t.Fatal(err)
		}
		if b.ExecutionEpoch == "" || b.ExecutionEpoch == a.ExecutionEpoch {
			t.Fatal("child birth reused epoch")
		}
	})
	t.Run("UnclaimedIncarnation", func(t *testing.T) {
		s := factory(t)
		a, err := s.CreateRun(testCtx(), "birth", "wf", nil)
		if err != nil {
			t.Fatal(err)
		}
		if a.ExecutionEpoch == "" {
			t.Fatal("birth has no epoch")
		}
		if err := s.DeleteRun(testCtx(), a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PruneDeletionMarkers(testCtx(), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		b, err := s.CreateRun(testCtx(), a.ID, "successor", nil)
		if err != nil {
			t.Fatal(err)
		}
		if b.ExecutionEpoch == "" || b.ExecutionEpoch == a.ExecutionEpoch {
			t.Fatal("reused birth identity")
		}
		if _, ok, err := store.AsRunExecutionStore(s).AcquireRunExecutionIf(testCtx(), a.ID, executionExpected(a), store.ExecutionStart); ok || err != nil {
			t.Fatalf("stale birth claim %v %v", ok, err)
		}
	})
	t.Run("ReservationWireIdentity", func(t *testing.T) {
		s := factory(t)
		a := correctionStart(t, s, "wire-identity")
		c := store.AsOutputCorrectionCASStore(s)
		e := correctionCreate(t, s, a, "node")
		next := e
		next.Revision++
		next.Attempts++
		now := time.Now()
		next.Reservations = []store.OutputCorrectionReservation{{ID: "attempt", ExecutionEpoch: a.Epoch, ScopeKey: "scope", StartedAt: now}}
		if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", store.ExpectOutputCorrection(e), next); err != nil || !ok {
			t.Fatalf("reserve %v %v", ok, err)
		}
		if next.Reservations[0].StartedAt != now {
			t.Fatal("store mutated submitted reservation")
		}
		durable := correctionLoad(t, s, a.RunID).OutputCorrections["node"]
		if got := durable.Reservations[0].StartedAt; !got.Equal(now.UTC().Truncate(time.Millisecond)) {
			t.Fatalf("timestamp not wire-canonical: %v", got)
		}
		changes := []struct {
			name   string
			change func(*store.OutputCorrectionReservation)
		}{
			{"ID", func(r *store.OutputCorrectionReservation) { r.ID = "other" }},
			{"Scope", func(r *store.OutputCorrectionReservation) { r.ScopeKey = "other" }},
			{"Epoch", func(r *store.OutputCorrectionReservation) { r.ExecutionEpoch = "other" }},
			{"Receipt", func(r *store.OutputCorrectionReservation) { r.Receipt = &store.OutputCorrectionReceipt{} }},
			{"Timestamp", func(r *store.OutputCorrectionReservation) { r.StartedAt = r.StartedAt.Add(time.Millisecond) }},
		}
		for _, tc := range changes {
			t.Run(tc.name, func(t *testing.T) {
				bad := next
				bad.Revision++
				bad.Status = "succeeded"
				bad.Reservations = append([]store.OutputCorrectionReservation(nil), next.Reservations...)
				tc.change(&bad.Reservations[0])
				if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", store.ExpectOutputCorrection(next), bad); ok || !errors.Is(err, store.ErrInvalidOutputCorrection) {
					t.Fatalf("changed reservation accepted: %v %v", ok, err)
				}
			})
		}
		verdict := next
		verdict.Revision++
		verdict.Status = "succeeded"
		if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", store.ExpectOutputCorrection(next), verdict); err != nil || !ok {
			t.Fatalf("unchanged submitted reservation refused: %v %v", ok, err)
		}
	})
	for _, legacy := range []bool{false, true} {
		name := "RetiredGenerationNew"
		if legacy {
			name = "RetiredGenerationLegacy"
		}
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			a := correctionStart(t, s, "history")
			c := store.AsOutputCorrectionCASStore(s)
			old := correctionReserve(t, s, a, "node", correctionCreate(t, s, a, "node"))
			r := correctionLoad(t, s, a.RunID)
			r.OutputCorrectionHistory = append(r.OutputCorrectionHistory, old)
			delete(r.OutputCorrections, "node")
			expected := store.OutputCorrectionExpected{Absent: true}
			next := store.OutputCorrectionEpisode{Generation: old.Generation, Revision: 1, Status: "active", Budget: 1}
			if legacy {
				base := store.OutputCorrectionEpisode{Status: "active", Budget: 1}
				r.OutputCorrections["node"] = base
				expected = store.ExpectOutputCorrection(base)
			}
			if err := s.SaveRun(testCtx(), r); err != nil {
				t.Fatal(err)
			}
			if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", expected, next); ok || !errors.Is(err, store.ErrInvalidOutputCorrection) {
				t.Fatalf("retired generation reused: %v %v", ok, err)
			}
			next.Generation = store.NewExecutionEpoch()
			if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", expected, next); err != nil || !ok {
				t.Fatalf("new generation refused: %v %v", ok, err)
			}
			bill := store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{CostUSD: 7, Tokens: 19}}
			if ok, err := c.RecordOutputCorrectionReceipt(testCtx(), a.RunID, old.Reservations[0].ReceiptTarget("node", old.Generation), bill); err != nil || !ok {
				t.Fatalf("known history receipt refused: %v %v", ok, err)
			}
			final := correctionLoad(t, s, a.RunID)
			if final.OutputCorrections["node"].Usage != (store.OutputCorrectionUsage{}) || final.OutputCorrectionHistory[0].Usage != bill.Usage {
				t.Fatal("receipt credited successor instead of history")
			}
		})
	}
}
