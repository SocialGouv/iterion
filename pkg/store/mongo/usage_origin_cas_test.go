package mongo

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageOriginMongoFreezeBeforeCAS(t *testing.T) {
	s := correctionMongoStore(t)
	ctx, cancel := context.WithTimeout(store.WithTenant(context.Background(), "origin-team"), 20*time.Second)
	defer cancel()
	r, err := s.CreateRun(ctx, "origin-cas", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, mongoExecutionExpected(r), store.ExecutionStart)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	origin := store.UsageOrigin{Version: 1, RunID: r.ID, Publication: "2026-11-04T00:00:00Z", TenantID: "origin-team", OrgID: "org"}
	res := store.OutputCorrectionReservation{ID: "initial", ExecutionEpoch: a.Epoch, ScopeKey: r.ID, StartedAt: time.Now(), Origin: &origin}
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
	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() { ok, e := watched.ReserveInitialUsageIf(ctx, r.ID, a, res); done <- result{ok, e} }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first reservation CAS never started")
	}
	fresh, err := s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Name = "metadata update"
	if err = s.SaveRun(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	origin.OrgID = "caller-reused-buffer"
	unblock()
	got := <-done
	if got.err != nil || !got.ok {
		t.Fatal(got)
	}
	final, err := s.LoadRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updates.Load() != 2 || final.InitialUsageReservations[res.ID].Origin.OrgID != "org" || final.ExecutionEpoch != a.Epoch {
		t.Fatalf("origin changed across CAS retries=%d stored=%+v", updates.Load(), final.InitialUsageReservations[res.ID])
	}
	t.Log("reservation CAS retried twice; copied origin remained original; metadata update preserved authority")
}
