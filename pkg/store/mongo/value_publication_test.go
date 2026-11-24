package mongo

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	drivermongo "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This is a second real client, with a command observer before an actual wire
// operation. No store operation or server result is mocked.
func valueMonitoredClient(t *testing.T, s *Store, monitor *event.CommandMonitor) *Store {
	t.Helper()
	client, e := drivermongo.Connect(options.Client().ApplyURI(os.Getenv("ITERION_TEST_MONGO_URI")).SetRegistry(Registry()).SetMonitor(monitor))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	db := client.Database(s.db.Name())
	return &Store{client: client, db: db, runs: db.Collection(colRuns), events: db.Collection(colEvents), interactions: db.Collection(colInteractions), runSeq: db.Collection(colRunSeq), blob: s.blob, logger: s.logger}
}
func TestValueRecordsMongoParentWriteFencesPublication(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"event", "interaction", "checkpoint"} {
		t.Run(kind, func(t *testing.T) {
			s, c := valueFormatMongo(t, true)
			if _, e := s.CreateRun(c, "r", "legacy", nil); e != nil {
				t.Fatal(e)
			}
			held, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var parentWrites atomic.Int32
			monitor := &event.CommandMonitor{Started: func(_ context.Context, ev *event.CommandStartedEvent) {
				if ev.Command.Lookup("autocommit").Type == 0 {
					return
				}
				isParent := ev.CommandName == "update" && ev.Command.Lookup("update").StringValue() == colRuns
				if isParent {
					parentWrites.Add(1)
				}
				isChild := (ev.CommandName == "insert" && ev.Command.Lookup("insert").StringValue() == colEvents) || (ev.CommandName == "update" && ev.Command.Lookup("update").StringValue() == colInteractions)
				if isParent || isChild {
					once.Do(func() { close(held); <-release })
				}
			}}
			writer := valueMonitoredClient(t, s, monitor)
			done := make(chan error, 1)
			go func() {
				var e error
				switch kind {
				case "event":
					_, e = writer.AppendEvent(c, "r", store.Event{Type: "old"})
				case "interaction":
					e = writer.WriteInteraction(c, &store.Interaction{RunID: "r", ID: "old"})
				case "checkpoint":
					e = writer.SaveCheckpoint(c, "r", &store.Checkpoint{Vars: map[string]any{"old": true}})
				}
				done <- e
			}()
			select {
			case <-held:
			case e := <-done:
				t.Fatal("writer never reached publication barrier", e)
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			// Release on every fatal path, before dropping the fixture.
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			if e := s.DeleteRun(c, "r"); e != nil {
				t.Fatal(e)
			}
			if n, e := s.PruneDeletionMarkers(c, time.Now().Add(time.Hour)); e != nil || n != 1 {
				t.Fatal(n, e)
			}
			if _, e := s.CreateRunWithValueFormat(c, mongoExactBirth("r")); e != nil {
				t.Fatal(e)
			}
			exact := mongoValueBind(t, c, "r", 2)
			if _, e := s.AppendEvent(exact, "r", store.Event{Type: "successor", Data: mongoValuePayload()}); e != nil {
				t.Fatal(e)
			}
			unblock()
			if e := <-done; !errors.Is(e, store.ErrValueFormatChanged) {
				t.Fatalf("old %s publication accepted after interformat replacement: %v", kind, e)
			}
			es, e := s.LoadEvents(exact, "r")
			if e != nil || len(es) != 1 || es[0].Type != "successor" {
				t.Fatal(es, e)
			}
			r, e := s.LoadRun(exact, "r")
			if e != nil || r.Checkpoint != nil {
				t.Fatal(r, e)
			}
			ids, e := s.ListInteractions(exact, "r")
			if e != nil || len(ids) != 0 {
				t.Fatal(ids, e)
			}
			if parentWrites.Load() == 0 {
				t.Fatal("no parent write observed")
			}
		})
	}
}
func TestValueRecordsMongoAbsentKeyFencesBirth(t *testing.T) {
	t.Parallel()
	s, c := valueFormatMongo(t, true)
	// Keep the actual orphan transaction open immediately before child insertion;
	// its parent INSERT+DELETE has already reached the server.
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	writer := valueMonitoredClient(t, s, &event.CommandMonitor{Started: func(_ context.Context, ev *event.CommandStartedEvent) {
		if ev.CommandName == "insert" && ev.Command.Lookup("insert").StringValue() == colEvents {
			once.Do(func() { close(held); <-release })
		}
	}})
	done := make(chan error, 1)
	go func() { _, e := writer.AppendEvent(c, "r", store.Event{Type: "orphan"}); done <- e }()
	<-held
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	session, e := s.client.StartSession()
	if e != nil {
		t.Fatal(e)
	}
	defer session.EndSession(c)
	if e = session.StartTransaction(); e != nil {
		t.Fatal(e)
	}
	sc := drivermongo.NewSessionContext(c, session)
	_, e = s.runs.InsertOne(sc, bson.M{"_id": "r", "tenant_id": "format-tenant", "format_version": 2})
	var serverError drivermongo.ServerError
	if !errors.As(e, &serverError) || !serverError.HasErrorLabel("TransientTransactionError") {
		t.Fatalf("absent parent key not exclusively reserved: %T %v", e, e)
	}
	_ = session.AbortTransaction(c)
	unblock()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRunWithValueFormat(c, mongoExactBirth("r")); !errors.Is(e, store.ErrInvalidValueFormat) {
		t.Fatal("exact birth ignored committed orphan", e)
	}
	if n, e := s.runs.CountDocuments(c, bson.M{}); e != nil || n != 0 {
		t.Fatal("temporary parent escaped", n, e)
	}
}
func TestValueRecordsMongoForeignTenantCannotReserveKey(t *testing.T) {
	t.Parallel()
	s, c := valueFormatMongo(t, true)
	if _, e := s.CreateRun(c, "r", "owner", nil); e != nil {
		t.Fatal(e)
	}
	other := store.WithIdentity(t.Context(), "foreign", "tester")
	if _, e := s.AppendEvent(other, "r", store.Event{Type: "foreign"}); !drivermongo.IsDuplicateKeyError(e) {
		t.Fatalf("foreign key was not a definitive duplicate: %v", e)
	}
	n, e := s.events.CountDocuments(c, bson.M{})
	if e != nil || n != 0 {
		t.Fatal(n, e)
	}
	if _, e = s.CreateRunWithValueFormat(other, mongoExactBirth("r")); e == nil {
		t.Fatal("foreign birth accepted")
	}
}

func TestValueRecordsMongoLegacyCounterOrdersConverge(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"canonical", "reversed", "both"} {
		t.Run(mode, func(t *testing.T) {
			s, c := mongoValueStore(t)
			canonical := bson.D{{Key: "tenant_id", Value: "format-tenant"}, {Key: "run_id", Value: "r"}}
			reversed := bson.D{{Key: "run_id", Value: "r"}, {Key: "tenant_id", Value: "format-tenant"}}
			if mode != "reversed" {
				if _, e := s.runSeq.InsertOne(c, bson.M{"_id": canonical, "next_seq": int64(4), planSeqField: int64(20)}); e != nil {
					t.Fatal(e)
				}
			}
			if mode != "canonical" {
				if _, e := s.runSeq.InsertOne(c, bson.M{"_id": reversed, "next_seq": int64(8), planSeqField: int64(11)}); e != nil {
					t.Fatal(e)
				}
			}
			foreign := bson.D{{Key: "tenant_id", Value: "foreign"}, {Key: "run_id", Value: "r"}}
			if _, e := s.runSeq.InsertOne(c, bson.M{"_id": foreign, "next_seq": int64(999), planSeqField: int64(999)}); e != nil {
				t.Fatal(e)
			}

			firstExpected := int64(8)
			if mode == "canonical" {
				firstExpected = 4
			}
			first, firstErr := s.AppendEvent(c, "r", store.Event{Type: "from-legacy-counter"})
			if firstErr != nil || first.Seq != firstExpected {
				t.Fatal("legacy event counter regressed", first, firstErr)
			}
			// A persisted event tail dominates either legacy counter without confusing
			// the independent plan counter. No payload is fabricated: use the real codec.
			raw, e := valueRecordDocument(c, 2, store.ExactEvent, &store.Event{RunID: "r", Seq: 30, TenantID: "format-tenant", Type: "tail"})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.events.InsertOne(c, raw); e != nil {
				t.Fatal(e)
			}
			ev, e := s.AppendEvent(c, "r", store.Event{Type: "after-tail"})
			if e != nil || ev.Seq != 31 {
				t.Fatal("tail not restored", ev, e)
			}
			nextPlan := int64(20)
			if mode == "reversed" {
				nextPlan = 11
			}
			plan, added, e := s.AppendPlanSnapshot(c, "r", store.PlanSnapshot{Todos: []store.PlanTodo{{Content: "legacy-counter-control"}}})
			if e != nil || !added || int64(plan.Seq) != nextPlan {
				t.Fatal("plan counter lost or coupled to event", plan, added, e)
			}
			other := valueMonitoredClient(t, s, nil)
			var wg sync.WaitGroup
			results := make(chan int64, 16)
			errs := make(chan error, 16)
			for j := 0; j < 16; j++ {
				st := s
				if j%2 == 1 {
					st = other
				}
				wg.Go(func() {
					ev, e := st.AppendEvent(c, "r", store.Event{Type: "concurrent"})
					if e != nil {
						errs <- e
					} else {
						results <- ev.Seq
					}
				})
			}
			wg.Wait()
			close(errs)
			close(results)
			for e := range errs {
				t.Fatal(e)
			}
			seen := map[int64]bool{}
			for n := range results {
				if n < 32 || n >= 48 || seen[n] {
					t.Fatal("duplicate or regressed sequence", n, seen)
				}
				seen[n] = true
			}
			if len(seen) != 16 {
				t.Fatal(seen)
			}
			var counter struct {
				Event int64 `bson:"next_seq"`
				Plan  int64 `bson:"next_plan_seq"`
			}
			if e = s.runSeq.FindOne(c, bson.M{"_id": canonical}).Decode(&counter); e != nil || counter.Event != 48 || counter.Plan != nextPlan+1 {
				t.Fatal(counter, e)
			}
			if e = s.runSeq.FindOne(c, bson.M{"_id": foreign}).Decode(&counter); e != nil || counter.Event != 999 || counter.Plan != 999 {
				t.Fatal("foreign counter changed", counter, e)
			}
		})
	}
}
