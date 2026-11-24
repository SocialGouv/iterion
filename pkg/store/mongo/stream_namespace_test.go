package mongo

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"os"
	"sync/atomic"
	"testing"
)

func TestMongoStreamStorageNamespaces(t *testing.T) {
	for _, f := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			requireStreamProtocol(t, store.IncarnationRunData)
			s, ctx := valueFormatMongo(t, true)
			s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
			if e := s.EnsureSchema(ctx, 0); e != nil {
				t.Fatal(e)
			}
			create := func() (context.Context, *store.Run) {
				r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "streams", WorkflowName: "wf", Format: f, DataProtocol: store.IncarnationRunData})
				if e != nil {
					t.Fatal(e)
				}
				c, e := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: r.ID, Format: f})
				if e != nil {
					t.Fatal(e)
				}
				b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: f, Protocol: r.DataProtocol, BirthID: r.BirthID})
				if e != nil {
					t.Fatal(e)
				}
				c, e = store.WithRunDataBinding(c, b)
				if e != nil {
					t.Fatal(e)
				}
				return c, r
			}
			ca, a := create()
			na, e := s.streamNamespace(ca, a.ID, a)
			if e != nil {
				t.Fatal(e)
			}
			for i := 0; i < 129; i++ {
				if _, e := s.AppendEvent(ca, a.ID, store.Event{}); e != nil {
					t.Fatal(e)
				}
			}
			if e := s.AppendRunLog(ca, a.ID, 0, []byte("A")); e != nil {
				t.Fatal(e)
			}
			snap := store.PlanSnapshot{Todos: []store.PlanTodo{{}}}
			if _, ok, e := s.AppendPlanSnapshot(ca, a.ID, snap); e != nil || !ok {
				t.Fatalf("plan %v %v", ok, e)
			}
			// A rolled-back allocation must not advance the committed sequence.
			session, e := s.client.StartSession()
			if e != nil {
				t.Fatal(e)
			}
			sentinel := errors.New("abort")
			_, e = session.WithTransaction(ca, func(sc context.Context) (any, error) {
				if _, e := s.allocStreamSeqField(sc, na, "next_seq"); e != nil {
					return nil, e
				}
				return nil, sentinel
			})
			session.EndSession(ca)
			if !errors.Is(e, sentinel) {
				t.Fatal(e)
			}
			next, e := s.AppendEvent(ca, a.ID, store.Event{})
			if e != nil || next.Seq != 129 {
				t.Fatalf("rollback seq %v %v", next, e)
			}
			// An append in every callback must not extend the initial inclusive ceiling.
			visited := 0
			e = s.ScanEvents(ca, a.ID, func(*store.Event) bool {
				visited++
				_, err := s.AppendEvent(ca, a.ID, store.Event{})
				if err != nil {
					t.Fatal(err)
				}
				return visited < 400
			})
			if e != nil || visited != 130 {
				t.Fatalf("finite scan %d %v", visited, e)
			}
			// Scope the failpoint to this reader's unique application name.
			app := "stream-retry-" + bsonNonce(t)
			var retryFailures atomic.Int64
			monitor := &event.CommandMonitor{Failed: func(_ context.Context, e *event.CommandFailedEvent) {
				if e.CommandName == "find" {
					retryFailures.Add(1)
				}
			}}
			client, err := mongo.Connect(options.Client().ApplyURI(os.Getenv("ITERION_TEST_MONGO_URI")).SetRegistry(Registry()).SetAppName(app).SetMonitor(monitor))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Disconnect(context.Background())
			db := client.Database(s.db.Name())
			reader := &Store{client: client, db: db, runs: db.Collection(colRuns)}
			defer func() {
				_ = s.client.Database("admin").RunCommand(context.Background(), bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}}).Err()
			}()
			// Force a transient find failure in page two; callbacks already delivered
			// must not be repeated by the driver's transaction retry.
			visited = 0
			e = reader.ScanEvents(ca, a.ID, func(*store.Event) bool {
				visited++
				if visited == 128 {
					cmd := bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: bson.M{"times": 1}}, {Key: "data", Value: bson.M{"appName": app, "failCommands": bson.A{"find"}, "errorCode": 112, "errorLabels": bson.A{"TransientTransactionError"}}}}
					if err := s.client.Database("admin").RunCommand(ctx, cmd).Err(); err != nil {
						t.Fatal(err)
					}
				}
				return true
			})
			if e != nil || visited != 260 || retryFailures.Load() != 1 {
				t.Fatalf("visitor retry callbacks=%d failures=%d err=%v", visited, retryFailures.Load(), e)
			}
			// A replacement after the first page cannot be observed by the second page.
			var cb context.Context
			var b *store.Run
			visited = 0
			e = s.ScanEvents(ca, a.ID, func(*store.Event) bool {
				visited++
				if visited == 128 {
					if _, err := s.runs.DeleteOne(ctx, bson.M{"_id": a.ID}); err != nil {
						t.Fatal(err)
					}
					cb, b = create()
					if _, err := s.AppendEvent(cb, b.ID, store.Event{}); err != nil {
						t.Fatal(err)
					}
					if err := s.AppendRunLog(cb, b.ID, 0, []byte("B")); err != nil {
						t.Fatal(err)
					}
					if _, ok, err := s.AppendPlanSnapshot(cb, b.ID, snap); err != nil || !ok {
						t.Fatalf("B dedup must be independent %v %v", ok, err)
					}
				}
				return true
			})
			if !errors.Is(e, store.ErrRunDataIdentityChanged) || visited != 128 {
				t.Fatalf("replacement scan %d %v", visited, e)
			}
			nb, e := s.streamNamespace(cb, b.ID, b)
			if e != nil {
				t.Fatal(e)
			}
			if na.namespace == nb.namespace {
				t.Fatal("shared namespace")
			}
			if _, e := s.LoadEvents(ca, a.ID); !errors.Is(e, store.ErrRunDataIdentityChanged) {
				t.Fatalf("stale events %v", e)
			}
			if _, e := s.ListPlanSnapshots(ca, a.ID); !errors.Is(e, store.ErrRunDataIdentityChanged) {
				t.Fatalf("stale plans %v", e)
			}
			// A seq-zero-only stream is nonempty; the inclusive maximum may be MaxInt64.
			countVisits := 0
			if err := s.ScanEvents(cb, b.ID, func(*store.Event) bool { countVisits++; return true }); err != nil || countVisits != 1 {
				t.Fatalf("seq0 scan %d %v", countVisits, err)
			}
			rawDoc, err := valueRecordDocument(cb, f, store.ExactEvent, &store.Event{RunID: b.ID, Seq: int64(9223372036854775807)})
			if err != nil {
				t.Fatal(err)
			}
			nb.stamp(rawDoc)
			if _, err = nb.events.InsertOne(cb, rawDoc); err != nil {
				t.Fatal(err)
			}
			countVisits = 0
			if err := s.ScanEvents(cb, b.ID, func(*store.Event) bool { countVisits++; return true }); err != nil || countVisits != 2 {
				t.Fatalf("max seq scan %d %v", countVisits, err)
			}
			countVisits = 0
			if err := s.ScanEvents(cb, b.ID, func(*store.Event) bool { countVisits++; return false }); err != nil || countVisits != 1 {
				t.Fatalf("early stop %d %v", countVisits, err)
			}
			// Privileged access still derives the tenant from the retained metadata.
			privileged := store.WithoutTenantFilter(t.Context())
			bind, _ := store.RunDataBindingFrom(cb)
			privileged, e = store.WithRunDataBinding(privileged, bind)
			if e != nil {
				t.Fatal(e)
			}
			if raw, e := s.ReadRunLogRange(privileged, b.ID, 0, 0); e != nil || string(raw) != "B" {
				t.Fatalf("privileged %q %v", raw, e)
			}
			// Legacy granular cleanup cannot touch either strict birth.
			for _, c := range []*mongo.Collection{s.events, s.runLogs, s.runPlans} {
				if _, e := c.DeleteMany(ctx, bson.M{"run_id": a.ID}); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := s.runSeq.DeleteMany(ctx, bson.M{"_id.run_id": a.ID}); e != nil {
				t.Fatal(e)
			}
			for _, n := range []streamDataNamespace{na, nb} {
				for _, c := range []*mongo.Collection{n.events, n.logs, n.plans} {
					count, e := c.CountDocuments(ctx, n.filter(ctx, nil))
					if e != nil || count == 0 {
						t.Fatalf("lost retained rows %s %d %v", c.Name(), count, e)
					}
				}
				count, e := n.seq.CountDocuments(ctx, bson.M{"_id": n.counterKey(ctx)})
				if e != nil || count != 1 {
					t.Fatalf("counter %d %v", count, e)
				}
			}
			if e := s.DeleteRun(cb, b.ID); e != nil {
				t.Fatal(e)
			}
			for _, c := range []*mongo.Collection{nb.events, nb.logs, nb.plans} {
				count, e := c.CountDocuments(ctx, nb.filter(ctx, nil))
				if e != nil || count != 0 {
					t.Fatalf("B cleanup %d %v", count, e)
				}
				count, e = c.CountDocuments(ctx, na.filter(ctx, nil))
				if e != nil || count == 0 {
					t.Fatalf("A collateral cleanup %d %v", count, e)
				}
			}
		})
	}
}
func TestMongoStreamOpaqueCompatibility(t *testing.T) {
	for _, f := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			s, ctx := valueFormatMongo(t, true)
			c, e := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "absent", Format: f})
			if e != nil {
				t.Fatal(e)
			}
			if _, e := s.runLogs.InsertOne(ctx, bson.M{"run_id": "absent", "tenant_id": "format-tenant", "offset": int64(0), "data": []byte("orphan")}); e != nil {
				t.Fatal(e)
			}
			if data, e := s.ReadRunLogRange(c, "absent", 1, 4); e != nil || string(data) != "rph" {
				t.Fatalf("opaque absent %q %v", data, e)
			}
			if size, e := s.RunLogSize(c, "absent"); e != nil || size != 6 {
				t.Fatalf("opaque size %d %v", size, e)
			}
			canceled, cancel := context.WithCancel(c)
			cancel()
			if _, e := s.ReadRunLogRange(canceled, "absent", 0, 0); !errors.Is(e, context.Canceled) {
				t.Fatalf("cancel %v", e)
			}
			if _, e := s.ReadRunLogRange(c, "other", 0, 0); e == nil {
				t.Fatal("run ID mismatch accepted")
			}
			for _, proto := range []store.RunDataProtocol{0, 1} {
				t.Run(fmt.Sprintf("protocol%d", proto), func(t *testing.T) {
					requireStreamProtocol(t, proto)
					s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
					id := fmt.Sprintf("deleted%d", proto)
					r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: id, WorkflowName: "wf", Format: f, DataProtocol: proto})
					if e != nil {
						t.Fatal(e)
					}
					b, _ := store.CaptureRunDataBinding(store.RunDataHeader{RunID: id, Format: f, Protocol: proto, BirthID: r.BirthID})
					bound, e := store.WithRunDataBinding(ctx, b)
					if e != nil {
						t.Fatal(e)
					}
					if e = s.DeleteRun(bound, id); e != nil {
						t.Fatal(e)
					}
					_, e = s.ListPlanSnapshots(bound, id)
					if proto == 0 && e != nil {
						t.Fatalf("historical tombstone %v", e)
					}
					if proto == 1 && !errors.Is(e, store.ErrRunDeleted) {
						t.Fatalf("strict tombstone %v", e)
					}
					_, e = s.ReadRunLogRange(bound, id, 0, 0)
					if proto == 0 && e != nil {
						t.Fatalf("historical logs tombstone %v", e)
					}
					if proto == 1 && !errors.Is(e, store.ErrRunDeleted) {
						t.Fatalf("strict logs tombstone %v", e)
					}
				})
			}
		})
	}
}

func requireStreamProtocol(t *testing.T, protocol store.RunDataProtocol) {
	t.Helper()
	if protocol != store.IncarnationRunData {
		return
	}
	if err := store.RequireRunDataStorage(protocol); err != nil {
		if !errors.Is(err, store.ErrRunDataCapability) {
			t.Fatal(err)
		}
		t.Skip("incarnation payload admission is not enabled")
	}
}
