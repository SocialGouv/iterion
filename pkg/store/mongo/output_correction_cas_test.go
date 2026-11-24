package mongo

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func correctionMongoStore(t *testing.T) *Store {
	t.Helper()
	s := retryTestStore(t)
	t.Cleanup(func() { ctx, cancel := mongotest.TeardownCtx(); defer cancel(); _ = s.db.Drop(ctx) })
	return s
}

func mongoExecutionExpected(r *store.Run) store.RunExecutionExpectation {
	return store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}
}

// The driver's command monitor pauses the actual CAS before it reaches Mongo.
// A second real client changes the row; no production-only testing seam exists.
func TestOutputCorrectionMongoRecalculatesAfterConflict(t *testing.T) {
	for _, change := range []string{"metadata", "pause-resume"} {
		t.Run(change, func(t *testing.T) {
			s := correctionMongoStore(t)
			ctx, cancel := context.WithTimeout(retryCtx(), 20*time.Second)
			defer cancel()
			r, err := s.CreateRun(ctx, "cas", "workflow", nil)
			if err != nil {
				t.Fatal(err)
			}
			a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, mongoExecutionExpected(r), store.ExecutionStart)
			if err != nil || !ok {
				t.Fatalf("claim %v %v", ok, err)
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
				applied bool
				err     error
			}
			done := make(chan result, 1)
			go func() {
				ok, err := watched.SetRunOutputCorrectionIf(ctx, r.ID, a, "key", store.OutputCorrectionExpected{Absent: true}, store.OutputCorrectionEpisode{Generation: store.NewExecutionEpoch(), Revision: 1, Status: "active"})
				done <- result{ok, err}
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("CAS was not reached")
			}
			if change == "metadata" {
				fresh, err := s.LoadRun(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				fresh.Name = "benign"
				if err := s.SaveRun(ctx, fresh); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := s.PauseRun(ctx, r.ID, &store.Checkpoint{NodeID: "node"}); err != nil {
					t.Fatal(err)
				}
				fresh, err := s.LoadRun(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, mongoExecutionExpected(fresh), store.ExecutionResume); err != nil || !ok {
					t.Fatalf("resume %v %v", ok, err)
				}
			}
			once.Do(func() { close(release) })
			var got result
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("CAS did not finish")
			}
			if got.err != nil || got.applied != (change == "metadata") {
				t.Fatalf("after %s applied=%v err=%v", change, got.applied, got.err)
			}
			final, err := s.LoadRun(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (len(final.OutputCorrections) == 1) != (change == "metadata") {
				t.Fatalf("stale decision persisted after %s: %+v", change, final.OutputCorrections)
			}
		})
	}
}

func TestOutputCorrectionMongoLegacyAndTenant(t *testing.T) {
	s := correctionMongoStore(t)
	ctx := retryCtx()
	_, err := s.runs.InsertOne(ctx, bson.M{"_id": "legacy", "tenant_id": retryTenant, "status": "running"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if r.CASVersion != 0 || r.ExecutionEpoch != "" {
		t.Fatal("fixture is not legacy")
	}
	a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, mongoExecutionExpected(r), store.ExecutionStart)
	if err != nil || !ok || a.Epoch == "" {
		t.Fatalf("legacy acquisition %v %v", ok, err)
	}
	wrong := store.WithTenant(context.Background(), "other-tenant")
	r, err = s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AcquireRunExecutionIf(wrong, r.ID, mongoExecutionExpected(r), store.ExecutionStart); err != nil || ok {
		t.Fatalf("cross-tenant claim %v %v", ok, err)
	}
	e := store.OutputCorrectionEpisode{Generation: "generation", Revision: 1, Status: "active"}
	if ok, err := s.SetRunOutputCorrectionIf(wrong, r.ID, a, "key", store.OutputCorrectionExpected{Absent: true}, e); ok || !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("cross-tenant decision %v %v", ok, err)
	}
	if ok, err := s.RecordOutputCorrectionReceipt(wrong, r.ID, store.OutputCorrectionReceiptTarget{LedgerKey: "key", Generation: "generation", ReservationID: "reservation", ExecutionEpoch: "epoch"}, store.OutputCorrectionReceipt{}); ok || !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("cross-tenant receipt %v %v", ok, err)
	}
}
