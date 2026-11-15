package storetest

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func executionExpected(r *store.Run) store.RunExecutionExpectation {
	return store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}
}

func correctionLoad(t *testing.T, s store.RunStore, id string) *store.Run {
	t.Helper()
	r, err := s.LoadRun(testCtx(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func correctionStart(t *testing.T, s store.RunStore, id string) store.RunExecutionAuthority {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), id, "correction", nil); err != nil {
		t.Fatal(err)
	}
	c := store.AsOutputCorrectionCASStore(s)
	if c == nil {
		t.Fatal("missing durable correction capability")
	}
	a, ok, err := c.AcquireRunExecutionIf(testCtx(), id, executionExpected(correctionLoad(t, s, id)), store.ExecutionStart)
	if err != nil || !ok || a.Epoch == "" || a.RunID != id {
		t.Fatalf("claim=%+v applied=%v err=%v", a, ok, err)
	}
	return a
}

func correctionCreate(t *testing.T, s store.RunStore, a store.RunExecutionAuthority, key string) store.OutputCorrectionEpisode {
	t.Helper()
	e := store.OutputCorrectionEpisode{Generation: store.NewExecutionEpoch(), Revision: 1, EpisodeID: "node/root", Status: "active", Budget: 2}
	ok, err := store.AsOutputCorrectionCASStore(s).SetRunOutputCorrectionIf(testCtx(), a.RunID, a, key, store.OutputCorrectionExpected{Absent: true}, e)
	if err != nil || !ok {
		t.Fatalf("create applied=%v err=%v", ok, err)
	}
	return correctionLoad(t, s, a.RunID).OutputCorrections[key]
}

func correctionReserve(t *testing.T, s store.RunStore, a store.RunExecutionAuthority, key string, e store.OutputCorrectionEpisode) store.OutputCorrectionEpisode {
	t.Helper()
	next := e
	next.Revision++
	next.Attempts++
	next.Reservations = append(append([]store.OutputCorrectionReservation(nil), e.Reservations...), store.OutputCorrectionReservation{ID: store.NewExecutionEpoch(), ExecutionEpoch: a.Epoch, ScopeKey: "run#branch#root", StartedAt: time.Now().UTC().Truncate(time.Millisecond)})
	ok, err := store.AsOutputCorrectionCASStore(s).SetRunOutputCorrectionIf(testCtx(), a.RunID, a, key, store.ExpectOutputCorrection(e), next)
	if err != nil || !ok {
		t.Fatalf("reserve applied=%v err=%v", ok, err)
	}
	return correctionLoad(t, s, a.RunID).OutputCorrections[key]
}

// RunExecutionCorrectionConformance exercises the actual methods of both stores.
func RunExecutionCorrectionConformance(t *testing.T, factory Factory) {
	runCorrectionIncarnationConformance(t, factory)
	runCorrectionReceiptTargetConformance(t, factory)
	t.Run("ZeroBudgetDoesNotReserve", func(t *testing.T) {
		s := factory(t)
		a := correctionStart(t, s, "zero")
		c := store.AsOutputCorrectionCASStore(s)
		e := correctionCreate(t, s, a, "node")
		zero := e
		zero.Revision++
		zero.Budget = 0
		if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", store.ExpectOutputCorrection(e), zero); err != nil || !ok {
			t.Fatalf("set budget0 %v %v", ok, err)
		}
		zero = correctionLoad(t, s, a.RunID).OutputCorrections["node"]
		next := zero
		next.Revision++
		next.Attempts++
		next.Reservations = []store.OutputCorrectionReservation{{ID: "attempt", ExecutionEpoch: a.Epoch, ScopeKey: "scope", StartedAt: time.Now().UTC()}}
		if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", store.ExpectOutputCorrection(zero), next); ok || !errors.Is(err, store.ErrInvalidOutputCorrection) {
			t.Fatalf("reserved against zero budget %v %v", ok, err)
		}
		if correctionLoad(t, s, a.RunID).OutputCorrections["node"].Attempts != 0 {
			t.Fatal("rejected reservation consumed attempt")
		}
	})
	t.Run("ReceiptConflictsWithVerdict", func(t *testing.T) {
		s := factory(t)
		a := correctionStart(t, s, "live-receipt")
		c := store.AsOutputCorrectionCASStore(s)
		e := correctionReserve(t, s, a, "node", correctionCreate(t, s, a, "node"))
		bill := store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{CostUSD: 2, Tokens: 7}}
		if ok, err := c.RecordOutputCorrectionReceipt(testCtx(), a.RunID, e.Reservations[0].ReceiptTarget("node", e.Generation), bill); err != nil || !ok {
			t.Fatalf("receipt %v %v", ok, err)
		}
		next := e
		next.Revision++
		next.Status = "succeeded"
		if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", store.ExpectOutputCorrection(e), next); err != nil || ok {
			t.Fatalf("stale verdict erased receipt %v %v", ok, err)
		}
		fresh := correctionLoad(t, s, a.RunID).OutputCorrections["node"]
		next = fresh
		next.Revision++
		next.Status = "succeeded"
		if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", store.ExpectOutputCorrection(fresh), next); err != nil || !ok {
			t.Fatalf("fresh verdict %v %v", ok, err)
		}
		final := correctionLoad(t, s, a.RunID).OutputCorrections["node"]
		if final.Usage != bill.Usage || final.Status != "succeeded" || final.Attempts != 1 {
			t.Fatalf("verdict lost bill: %+v", final)
		}
	})
	t.Run("MetadataAndClaims", func(t *testing.T) {
		s := factory(t)
		a := correctionStart(t, s, "meta")
		c := store.AsOutputCorrectionCASStore(s)
		before := correctionLoad(t, s, a.RunID)
		before.ExecutionEpoch = "forged"
		before.Name = "rename"
		if err := s.SaveRun(testCtx(), before); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveCheckpoint(testCtx(), a.RunID, &store.Checkpoint{NodeID: "node"}); err != nil {
			t.Fatal(err)
		}
		fresh := correctionLoad(t, s, a.RunID)
		if fresh.ExecutionEpoch != a.Epoch {
			t.Fatal("metadata revoked or forged authority")
		}
		stale := executionExpected(fresh)
		stale.Version--
		if _, ok, err := c.AcquireRunExecutionIf(testCtx(), a.RunID, stale, store.ExecutionStart); err != nil || ok {
			t.Fatalf("stale version claim %v %v", ok, err)
		}
		b, ok, err := c.AcquireRunExecutionIf(testCtx(), a.RunID, executionExpected(fresh), store.ExecutionStart)
		if err != nil || !ok || b.Epoch == a.Epoch {
			t.Fatalf("same-status acquisition did not renew: %v %v", ok, err)
		}
		// A returned grant remains its own value after a successor claims.
		if a.Matches(correctionLoad(t, s, a.RunID)) || a.Epoch == b.Epoch {
			t.Fatal("late claim adopted successor")
		}
		fresh = correctionLoad(t, s, a.RunID)
		wrong := executionExpected(fresh)
		wrong.Epoch = a.Epoch
		if _, ok, err := c.AcquireRunExecutionIf(testCtx(), a.RunID, wrong, store.ExecutionStart); err != nil || ok {
			t.Fatalf("refresh borrowed newer epoch %v %v", ok, err)
		}
	})
	t.Run("PauseABAAndExpectedAbsence", func(t *testing.T) {
		s := factory(t)
		a := correctionStart(t, s, "aba")
		c := store.AsOutputCorrectionCASStore(s)
		if err := s.PauseRun(testCtx(), a.RunID, &store.Checkpoint{NodeID: "node"}); err != nil {
			t.Fatal(err)
		}
		paused := correctionLoad(t, s, a.RunID)
		if paused.ExecutionEpoch == a.Epoch {
			t.Fatal("pause kept old epoch")
		}
		if err := s.PauseRun(testCtx(), a.RunID, &store.Checkpoint{NodeID: "node"}); err != nil {
			t.Fatal(err)
		}
		if correctionLoad(t, s, a.RunID).ExecutionEpoch != paused.ExecutionEpoch {
			t.Fatal("repeated pause revoked neutral metadata")
		}
		b, ok, err := c.AcquireRunExecutionIf(testCtx(), a.RunID, executionExpected(correctionLoad(t, s, a.RunID)), store.ExecutionResume)
		if err != nil || !ok || b.Epoch == a.Epoch {
			t.Fatalf("resume %v %v", ok, err)
		}
		e := store.OutputCorrectionEpisode{Generation: store.NewExecutionEpoch(), Revision: 1, Status: "active", Budget: 1}
		if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "absent", store.OutputCorrectionExpected{Absent: true}, e); err != nil || ok {
			t.Fatalf("old absent create passed %v %v", ok, err)
		}
		correctionCreate(t, s, b, "absent")
	})
	t.Run("DecisionCASAndReceipts", func(t *testing.T) {
		s := factory(t)
		a := correctionStart(t, s, "receipt")
		c := store.AsOutputCorrectionCASStore(s)
		e := correctionCreate(t, s, a, "node.with.$key")
		reserved := correctionReserve(t, s, a, "node.with.$key", e)
		stale := e
		stale.Revision++
		stale.Status = "succeeded"
		if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node.with.$key", store.ExpectOutputCorrection(e), stale); err != nil || ok {
			t.Fatalf("stale verdict passed %v %v", ok, err)
		}
		if err := s.PauseRun(testCtx(), a.RunID, &store.Checkpoint{NodeID: "node"}); err != nil {
			t.Fatal(err)
		}
		r := correctionLoad(t, s, a.RunID)
		r.OutputCorrectionHistory = append(r.OutputCorrectionHistory, reserved)
		delete(r.OutputCorrections, "node.with.$key")
		if err := s.SaveRun(testCtx(), r); err != nil {
			t.Fatal(err)
		}
		b, ok, err := c.AcquireRunExecutionIf(testCtx(), a.RunID, executionExpected(correctionLoad(t, s, a.RunID)), store.ExecutionResume)
		if err != nil || !ok {
			t.Fatal(err)
		}
		successor := correctionCreate(t, s, b, "node.with.$key")
		receipt := store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{Tokens: 17, CostUSD: 3, UnreportedCalls: 1}}
		if ok, err := c.RecordOutputCorrectionReceipt(testCtx(), a.RunID, reserved.Reservations[0].ReceiptTarget("node.with.$key", reserved.Generation), receipt); err != nil || !ok {
			t.Fatalf("late receipt %v %v", ok, err)
		}
		if ok, err := c.RecordOutputCorrectionReceipt(testCtx(), a.RunID, reserved.Reservations[0].ReceiptTarget("node.with.$key", reserved.Generation), receipt); err != nil || ok {
			t.Fatalf("duplicate receipt %v %v", ok, err)
		}
		r = correctionLoad(t, s, a.RunID)
		old := r.OutputCorrectionHistory[0]
		if old.Usage != receipt.Usage || old.Attempts != 1 || old.Status != "active" || !reflect.DeepEqual(r.OutputCorrections["node.with.$key"], successor) || r.ExecutionEpoch != b.Epoch {
			t.Fatalf("late receipt changed authority/verdict: %+v", r)
		}
		receipt.Usage.CostUSD++
		if _, err := c.RecordOutputCorrectionReceipt(testCtx(), a.RunID, reserved.Reservations[0].ReceiptTarget("node.with.$key", reserved.Generation), receipt); !errors.Is(err, store.ErrInvalidOutputCorrection) {
			t.Fatalf("contradictory receipt accepted %v", err)
		}
		if _, err := c.RecordOutputCorrectionReceipt(testCtx(), a.RunID, store.OutputCorrectionReceiptTarget{LedgerKey: "node.with.$key", Generation: successor.Generation, ReservationID: "unreserved", ExecutionEpoch: b.Epoch}, receipt); !errors.Is(err, store.ErrInvalidOutputCorrection) {
			t.Fatalf("unreserved receipt accepted %v", err)
		}
	})
	t.Run("ConcurrentReservation", func(t *testing.T) {
		s := factory(t)
		a := correctionStart(t, s, "race")
		c := store.AsOutputCorrectionCASStore(s)
		e := correctionCreate(t, s, a, "node")
		results := make(chan bool, 2)
		errs := make(chan error, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				<-start
				next := e
				next.Revision++
				next.Attempts++
				next.Reservations = []store.OutputCorrectionReservation{{ID: store.NewExecutionEpoch(), ExecutionEpoch: a.Epoch, ScopeKey: "race", StartedAt: time.Now().UTC()}}
				ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "node", store.ExpectOutputCorrection(e), next)
				results <- ok
				errs <- err
			})
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)
		wins := 0
		for ok := range results {
			if ok {
				wins++
			}
		}
		for err := range errs {
			if err != nil && !errors.Is(err, store.ErrRunConflict) {
				t.Fatal(err)
			}
		}
		if wins != 1 || correctionLoad(t, s, a.RunID).OutputCorrections["node"].Attempts != 1 {
			t.Fatalf("paid reservations=%d", wins)
		}
	})
	t.Run("QueuedRollbackAndReincarnation", func(t *testing.T) {
		s := factory(t)
		a := correctionStart(t, s, "queue")
		c := store.AsOutputCorrectionCASStore(s)
		if err := s.UpdateRunStatus(testCtx(), a.RunID, store.RunStatusPausedOperator, ""); err != nil {
			t.Fatal(err)
		}
		paused := correctionLoad(t, s, a.RunID)
		flipper := store.AsQueuedFlipper(s)
		flip, ok, err := flipper.FlipToQueued(testCtx(), a.RunID, paused.Status, time.Now().UTC())
		if err != nil || !ok {
			t.Fatal(err)
		}
		queued := correctionLoad(t, s, a.RunID)
		if queued.ExecutionEpoch == paused.ExecutionEpoch {
			t.Fatal("queue did not renew")
		}
		if ok, err := flipper.RevertQueuedFlip(testCtx(), a.RunID, flip, ""); err != nil || !ok {
			t.Fatalf("rollback %v %v", ok, err)
		}
		reverted := correctionLoad(t, s, a.RunID)
		if reverted.ExecutionEpoch == paused.ExecutionEpoch || reverted.ExecutionEpoch == queued.ExecutionEpoch {
			t.Fatal("rollback reused an epoch")
		}
		if err := s.DeleteRun(testCtx(), a.RunID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PruneDeletionMarkers(testCtx(), time.Now().UTC().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		b := correctionStart(t, s, a.RunID)
		if b.Epoch == a.Epoch {
			t.Fatal("reincarnation reused grant")
		}
		e := store.OutputCorrectionEpisode{Generation: store.NewExecutionEpoch(), Revision: 1, Status: "active"}
		if ok, err := c.SetRunOutputCorrectionIf(testCtx(), a.RunID, a, "absent", store.OutputCorrectionExpected{Absent: true}, e); err != nil || ok {
			t.Fatalf("old reincarnated grant %v %v", ok, err)
		}
	})
	for _, name := range []string{"save", "status", "conditional", "outcome", "failure", "terminal", "queued-claim", "queued-repeat"} {
		t.Run("Transition/"+name, func(t *testing.T) {
			s := factory(t)
			a := correctionStart(t, s, "transition")
			var err error
			switch name {
			case "save":
				r := correctionLoad(t, s, a.RunID)
				r.Status = store.RunStatusPausedOperator
				r.ExecutionEpoch = "forged"
				err = s.SaveRun(testCtx(), r)
			case "status":
				err = s.UpdateRunStatus(testCtx(), a.RunID, store.RunStatusCancelled, "")
			case "conditional":
				_, err = s.UpdateRunStatusIf(testCtx(), a.RunID, store.RunStatusPausedOperator, "", []store.RunStatus{store.RunStatusRunning})
			case "outcome":
				_, err = s.UpdateRunOutcome(testCtx(), a.RunID, store.RunStatusFailedResumable, "failed", store.RunOutcomeMeta{}, []store.RunStatus{store.RunStatusRunning})
			case "failure":
				err = s.FailRunResumable(testCtx(), a.RunID, &store.Checkpoint{NodeID: "node"}, "failed", "")
			case "terminal":
				err = s.FailRunTerminal(testCtx(), a.RunID, &store.Checkpoint{NodeID: "node"}, "failed", "")
			case "queued-claim", "queued-repeat":
				if err = s.UpdateRunStatus(testCtx(), a.RunID, store.RunStatusQueued, ""); err != nil {
					t.Fatal(err)
				}
				a.Epoch = correctionLoad(t, s, a.RunID).ExecutionEpoch
				if name == "queued-repeat" {
					err = s.UpdateRunStatus(testCtx(), a.RunID, store.RunStatusQueued, "")
				} else {
					_, err = store.AsQueuedAttemptClaimer(s).ClaimQueuedRunIfAttempt(testCtx(), a.RunID, time.Now().UTC().Add(time.Second))
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			after := correctionLoad(t, s, a.RunID)
			if after.ExecutionEpoch == "" || after.ExecutionEpoch == a.Epoch || after.ExecutionEpoch == "forged" {
				t.Fatalf("%s failed to renew epoch", name)
			}
		})
	}
}
