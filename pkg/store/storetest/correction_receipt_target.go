package storetest

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func runCorrectionReceiptTargetConformance(t *testing.T, factory Factory) {
	t.Run("ReceiptAfterCompleteRecreation", func(t *testing.T) {
		for _, reuse := range []bool{true, false} {
			name := "fresh-generation"
			if reuse {
				name = "reused-generation-and-reservation"
			}
			t.Run(name, func(t *testing.T) {
				s := factory(t)
				c := store.AsOutputCorrectionCASStore(s)
				create := func(generation string) (store.RunExecutionAuthority, store.OutputCorrectionEpisode) {
					a := correctionStart(t, s, "receipt-recreated")
					episode := store.OutputCorrectionEpisode{Generation: generation, Revision: 1, Status: "active", Budget: 1}
					if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", store.OutputCorrectionExpected{Absent: true}, episode); !ok || err != nil {
						t.Fatalf("create episode %v %v", ok, err)
					}
					next := episode
					next.Revision++
					next.Attempts = 1
					next.Reservations = []store.OutputCorrectionReservation{{ID: "attempt", ExecutionEpoch: a.Epoch, ScopeKey: "scope", StartedAt: time.Now()}}
					if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", store.ExpectOutputCorrection(episode), next); !ok || err != nil {
						t.Fatalf("reserve %v %v", ok, err)
					}
					return a, correctionLoad(t, s, a.RunID).OutputCorrections["node"]
				}
				old, episode := create("generation")
				target := episode.Reservations[0].ReceiptTarget("node", episode.Generation)
				if err := s.DeleteRun(testCtx(), old.RunID); err != nil {
					t.Fatal(err)
				}
				if n, err := s.PruneDeletionMarkers(testCtx(), time.Now().Add(time.Hour)); err != nil || n != 1 {
					t.Fatalf("prune %d %v", n, err)
				}
				generation := "generation"
				if !reuse {
					generation = store.NewExecutionEpoch()
				}
				current, fresh := create(generation)
				if current.Epoch == target.ExecutionEpoch {
					t.Fatal("fixture did not rotate reservation epoch")
				}
				before := correctionLoad(t, s, old.RunID)
				bill := store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{CostUSD: 7, Tokens: 19}}
				if ok, err := c.RecordOutputCorrectionReceipt(testCtx(), old.RunID, target, bill); ok || !errors.Is(err, store.ErrInvalidOutputCorrection) {
					t.Fatalf("old receipt credited successor: %v %v", ok, err)
				}
				if after := correctionLoad(t, s, old.RunID); !reflect.DeepEqual(before, after) {
					t.Fatal("rejected old receipt changed the successor")
				}
				freshTarget := fresh.Reservations[0].ReceiptTarget("node", fresh.Generation)
				if ok, err := c.RecordOutputCorrectionReceipt(testCtx(), current.RunID, freshTarget, bill); !ok || err != nil {
					t.Fatalf("new reservation's receipt refused: %v %v", ok, err)
				}
				if got := correctionLoad(t, s, current.RunID).OutputCorrections["node"]; got.Usage != bill.Usage {
					t.Fatalf("new receipt usage: %+v", got.Usage)
				}
			})
		}
	})
	t.Run("ReceiptTargetIdentityBeforeDuplicate", func(t *testing.T) {
		s := factory(t)
		c := store.AsOutputCorrectionCASStore(s)
		a := correctionStart(t, s, "receipt-identity")
		e := correctionReserve(t, s, a, "node", correctionCreate(t, s, a, "node"))
		original := e.Reservations[0].ReceiptTarget("node", e.Generation)
		bill := store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{CostUSD: 1, Tokens: 3}}
		for _, credited := range []bool{false, true} {
			if credited {
				if ok, err := c.RecordOutputCorrectionReceipt(testCtx(), a.RunID, original, bill); !ok || err != nil {
					t.Fatalf("original receipt refused: %v %v", ok, err)
				}
			}
			for _, field := range []string{"key", "generation", "reservation", "missing-epoch", "different-epoch"} {
				target := original
				switch field {
				case "key":
					target.LedgerKey = "other"
				case "generation":
					target.Generation = "other"
				case "reservation":
					target.ReservationID = "other"
				case "missing-epoch":
					target.ExecutionEpoch = ""
				case "different-epoch":
					target.ExecutionEpoch = store.NewExecutionEpoch()
				}
				before := correctionLoad(t, s, a.RunID)
				if ok, err := c.RecordOutputCorrectionReceipt(testCtx(), a.RunID, target, bill); ok || !errors.Is(err, store.ErrInvalidOutputCorrection) {
					t.Fatalf("credited=%v wrong %s accepted: %v %v", credited, field, ok, err)
				}
				if after := correctionLoad(t, s, a.RunID); !reflect.DeepEqual(before, after) {
					t.Fatalf("credited=%v wrong %s mutated run", credited, field)
				}
			}
		}
		if ok, err := c.RecordOutputCorrectionReceipt(testCtx(), a.RunID, original, bill); ok || err != nil {
			t.Fatalf("correct duplicate not a no-op: %v %v", ok, err)
		}
	})
}
