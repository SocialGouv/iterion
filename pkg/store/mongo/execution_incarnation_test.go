package mongo

import (
	"context"
	"errors"
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

func TestOutputCorrectionMongoReincarnationCAS(t *testing.T) {
	for _, operation := range []string{"decision", "receipt", "metadata-control"} {
		t.Run(operation, func(t *testing.T) {
			s := correctionMongoStore(t)
			ctx, cancel := context.WithTimeout(retryCtx(), 25*time.Second)
			defer cancel()
			r, err := s.CreateRun(ctx, "r17-reincarnated", "old-workflow", nil)
			if err != nil {
				t.Fatal(err)
			}
			old, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, mongoExecutionExpected(r), store.ExecutionStart)
			if err != nil || !ok {
				t.Fatalf("claim %v %v", ok, err)
			}
			episode := store.OutputCorrectionEpisode{Generation: "old-generation", Revision: 1, Status: "active", Budget: 1}
			if operation == "receipt" {
				if ok, err := s.SetRunOutputCorrectionIf(ctx, r.ID, old, "node", store.OutputCorrectionExpected{Absent: true}, episode); err != nil || !ok {
					t.Fatalf("create %v %v", ok, err)
				}
				next := episode
				next.Revision++
				next.Attempts = 1
				next.Reservations = []store.OutputCorrectionReservation{{ID: "old-attempt", ExecutionEpoch: old.Epoch, ScopeKey: "old-scope", StartedAt: time.Now().UTC().Truncate(time.Millisecond)}}
				if ok, err := s.SetRunOutputCorrectionIf(ctx, r.ID, old, "node", store.ExpectOutputCorrection(episode), next); err != nil || !ok {
					t.Fatalf("reserve %v %v", ok, err)
				}
			}
			before, err := s.LoadRun(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var armed atomic.Bool
			armed.Store(true)
			monitor := &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
				if e.CommandName == "update" && armed.CompareAndSwap(true, false) {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
			}}
			client, err := mongo.Connect(options.Client().ApplyURI(os.Getenv("ITERION_TEST_MONGO_URI")).SetRegistry(Registry()).SetMonitor(monitor))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Disconnect(context.Background()) }()
			watched := &Store{runs: client.Database(s.db.Name()).Collection(colRuns)}
			type result struct {
				ok  bool
				err error
			}
			done := make(chan result, 1)
			go func() {
				if operation == "receipt" {
					ok, err := watched.RecordOutputCorrectionReceipt(ctx, r.ID, store.OutputCorrectionReceiptTarget{LedgerKey: "node", Generation: "old-generation", ReservationID: "old-attempt", ExecutionEpoch: old.Epoch}, store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{CostUSD: 7, Tokens: 19}})
					done <- result{ok, err}
				} else {
					ok, err := watched.SetRunOutputCorrectionIf(ctx, r.ID, old, "node", store.OutputCorrectionExpected{Absent: true}, episode)
					done <- result{ok, err}
				}
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("CAS not reached")
			}
			successor := old
			if operation == "metadata-control" {
				before.Name = "metadata"
				if err := s.SaveRun(ctx, before); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := s.DeleteRun(ctx, r.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.PruneDeletionMarkers(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				recreated, err := s.CreateRun(ctx, r.ID, "successor-workflow", nil)
				if err != nil {
					t.Fatal(err)
				}
				successor, ok, err = s.AcquireRunExecutionIf(ctx, r.ID, mongoExecutionExpected(recreated), store.ExecutionStart)
				if err != nil || !ok {
					t.Fatalf("new claim %v %v", ok, err)
				}
				for {
					fresh, err := s.LoadRun(ctx, r.ID)
					if err != nil {
						t.Fatal(err)
					}
					if fresh.CASVersion >= before.CASVersion {
						if fresh.CASVersion != before.CASVersion {
							t.Fatal("fixture version overshot")
						}
						break
					}
					if err := s.SaveCheckpoint(ctx, r.ID, &store.Checkpoint{NodeID: "successor"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			once.Do(func() { close(release) })
			var got result
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("CAS hung")
			}
			final, err := s.LoadRun(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("operation=%s old-version=%d old-grant=%s successor-grant=%s final-grant=%s applied=%v err=%v corrections=%+v", operation, before.CASVersion, old.Epoch, successor.Epoch, final.ExecutionEpoch, got.ok, got.err, final.OutputCorrections)
			if operation == "metadata-control" {
				if got.err != nil || !got.ok {
					t.Fatalf("benign metadata refused: %v %v", got.ok, got.err)
				}
			} else if got.ok || len(final.OutputCorrections) != 0 {
				t.Fatalf("OLD OPERATION APPLIED TO NEW INCARNATION: operation=%s applied=%v err=%v", operation, got.ok, got.err)
			}
		})
	}
}

func TestOutputCorrectionMongoReservationTimestamp(t *testing.T) {
	s := correctionMongoStore(t)
	ctx := retryCtx()
	r, err := s.CreateRun(ctx, "r17-precision", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, mongoExecutionExpected(r), store.ExecutionStart)
	if err != nil || !ok {
		t.Fatal(err)
	}
	e := store.OutputCorrectionEpisode{Generation: "g", Revision: 1, Status: "active", Budget: 1}
	if ok, err := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.OutputCorrectionExpected{Absent: true}, e); err != nil || !ok {
		t.Fatalf("create %v %v", ok, err)
	}
	next := e
	next.Revision++
	next.Attempts = 1
	next.Reservations = []store.OutputCorrectionReservation{{ID: "attempt", ExecutionEpoch: a.Epoch, ScopeKey: "scope", StartedAt: time.Unix(1800000000, 123456789).UTC()}}
	if ok, err := s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.ExpectOutputCorrection(e), next); err != nil || !ok {
		t.Fatalf("reserve %v %v", ok, err)
	}
	r, err = s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	durable := r.OutputCorrections["node"]
	verdict := next
	verdict.Revision++
	verdict.Status = "succeeded"
	ok, err = s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.ExpectOutputCorrection(next), verdict)
	t.Logf("submitted=%s stored=%s expected-equal=%v verdict=%v err=%v", next.Reservations[0].StartedAt.Format(time.RFC3339Nano), durable.Reservations[0].StartedAt.Format(time.RFC3339Nano), store.ExpectOutputCorrection(next) == store.ExpectOutputCorrection(durable), ok, err)
	if !ok || err != nil {
		t.Errorf("ROUNDTRIP REFUSED UNCHANGED RESERVATION: %v %v", ok, err)
	}
	if !ok {
		verdict = durable
		verdict.Revision++
		verdict.Status = "succeeded"
		ok, err = s.SetRunOutputCorrectionIf(ctx, r.ID, a, "node", store.ExpectOutputCorrection(durable), verdict)
		t.Logf("reloaded control applied=%v err=%v", ok, err)
		if !ok || err != nil && !errors.Is(err, store.ErrInvalidOutputCorrection) {
			t.Fatalf("reloaded control=%v %v", ok, err)
		}
	}
}
