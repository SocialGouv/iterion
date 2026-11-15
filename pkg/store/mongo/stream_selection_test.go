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
	"math"
	"os"
	"sync/atomic"
	"testing"
)

func streamSelectionTestCreate(t *testing.T, s *Store, c context.Context, f store.ValueFormat, p store.RunDataProtocol) (*store.Run, context.Context) {
	t.Helper()
	if p == store.IncarnationRunData {
		if e := store.RequireRunDataStorage(p); e != nil {
			t.Skip("incarnation admission remains disabled")
		}
	}
	r, e := s.CreateRunWithValueFormat(c, store.RunCreateSpec{ID: "selected", WorkflowName: "wf", Format: f, DataProtocol: p})
	if e != nil {
		t.Fatal(e)
	}
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: f, Protocol: p, BirthID: r.BirthID})
	if e != nil {
		t.Fatal(e)
	}
	c, e = store.WithRunDataBinding(c, b)
	if e != nil {
		t.Fatal(e)
	}
	c, e = store.WithRunValueFormat(c, store.RunValueFormat{RunID: r.ID, Format: f})
	if e != nil {
		t.Fatal(e)
	}
	return r, c
}
func TestStreamSelectionOriginalObservation(t *testing.T) {
	for _, proto := range []store.RunDataProtocol{0, 1} {
		for _, f := range []store.ValueFormat{1, 2} {
			t.Run(fmt.Sprintf("p%df%d", proto, f), func(t *testing.T) {
				s, c := valueFormatMongo(t, true)
				s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
				a, ca := streamSelectionTestCreate(t, s, c, f, proto)
				sel, e := s.ResolveStreamSelection(c, a.ID)
				if e != nil {
					t.Fatalf("unbound resolve: %v", e)
				}
				if e = sel.Validate(c); e != nil {
					t.Fatal(e)
				}
				filter := sel.LogFilter()
				filter[0].Value = "other"
				if sel.LogFilter()[0].Value != a.ID {
					t.Fatal("mutable filter")
				}
				wrong := store.WithIdentity(t.Context(), "foreign", "tester")
				if e = sel.Validate(wrong); !errors.Is(e, store.ErrRunDataIdentityChanged) {
					t.Fatal(e)
				}
				priv, e := s.ResolveStreamSelection(store.WithoutTenantFilter(t.Context()), a.ID)
				if e != nil {
					t.Fatal(e)
				}
				found := false
				for _, v := range priv.LogFilter() {
					if v.Key == "tenant_id" && v.Value == "format-tenant" {
						found = true
					}
				}
				if !found {
					t.Fatal("privileged selection missing canonical tenant")
				}
				if _, e = s.runs.UpdateOne(c, bson.M{"_id": a.ID}, bson.M{"$set": bson.M{"status": "finished"}}); e != nil {
					t.Fatal(e)
				}
				if done, e := sel.Terminal(c); e != nil || !done {
					t.Fatal(done, e)
				}
				if _, e = s.runs.DeleteOne(c, bson.M{"_id": a.ID}); e != nil {
					t.Fatal(e)
				}
				b, cb := streamSelectionTestCreate(t, s, c, f, proto)
				if b.BirthID == a.BirthID {
					t.Fatal("samebirth")
				}
				if _, e = s.ResolveStreamSelection(ca, a.ID); !errors.Is(e, store.ErrRunDataIdentityChanged) {
					t.Fatalf("supplied stale binding adopted: %v", e)
				}
				if e = sel.Validate(cb); e == nil {
					t.Fatal("successor operation rebound selection")
				}
				if done, e := sel.Terminal(c); e == nil || done {
					t.Fatalf("successor terminal: %v %v", done, e)
				}
			})
		}
	}
}
func TestStreamSelectionAbsentObservation(t *testing.T) {
	for _, f := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			s, c := valueFormatMongo(t, true)
			cf, e := store.WithRunValueFormat(c, store.RunValueFormat{RunID: "selected", Format: f})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.runLogs.InsertOne(c, bson.M{"run_id": "selected", "tenant_id": "format-tenant", "offset": int64(0), "data": []byte("orphan")}); e != nil {
				t.Fatal(e)
			}
			sel, e := s.ResolveStreamSelection(cf, "selected")
			if e != nil {
				t.Fatal(e)
			}
			got := ""
			if e = sel.BackfillLogs(cf, 0, func(_ int64, b []byte) bool { got += string(b); return true }); e != nil || got != "orphan" {
				t.Fatal(got, e)
			}
			if f == 2 {
				e = sel.BackfillEvents(cf, 0, false, 1, func([]*store.Event) bool { t.Fatal("exact headerless callback"); return false })
				if e == nil {
					t.Fatal("headerless exact replay accepted")
				}
			}
			if _, e = s.CreateRunWithValueFormat(c, store.RunCreateSpec{ID: "selected", WorkflowName: "wf", Format: f}); e != nil {
				t.Fatal(e)
			}
			if e = sel.Validate(cf); !errors.Is(e, store.ErrRunDataIdentityChanged) {
				t.Fatalf("appeared parent adopted: %v", e)
			}
		})
	}
}
func TestStreamSelectionLogExtentClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		off  any
		data any
		bad  bool
	}{{"negative", int64(-1), []byte("x"), true}, {"overflow", int64(math.MaxInt64), []byte("x"), true}, {"maximum", int64(math.MaxInt64 - 1), []byte("x"), false}, {"malformed-offset", "bad", []byte("x"), true}, {"malformed-data", int64(0), int64(7), true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := valueFormatMongo(t, true)
			r, b := streamSelectionTestCreate(t, s, c, 1, 0)
			sel, e := s.ResolveStreamSelection(b, r.ID)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = sel.LogsCollection().InsertOne(c, bson.M{"run_id": r.ID, "tenant_id": "format-tenant", "offset": tc.off, "data": tc.data}); e != nil {
				t.Fatal(e)
			}
			calls := 0
			e = sel.BackfillLogs(c, 0, func(int64, []byte) bool { calls++; return true })
			if tc.bad {
				if !errors.Is(e, store.ErrEventsCorrupted) || calls != 0 {
					t.Fatalf("deterministic corrupt tail: calls%d err%T %v", calls, e, e)
				}
			} else if e != nil || calls != 1 {
				t.Fatal(calls, e)
			}
		})
	}
}
func TestStreamSelectionMissingParent(t *testing.T) {
	s, c := valueFormatMongo(t, true)
	s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
	r, _ := streamSelectionTestCreate(t, s, c, 2, 1)
	sel, e := s.ResolveStreamSelection(c, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.runs.DeleteOne(c, bson.M{"_id": r.ID}); e != nil {
		t.Fatal(e)
	}
	if e = sel.Validate(c); !errors.Is(e, store.ErrRunNotFound) {
		t.Fatalf("missing Validate %T %v", e, e)
	}
	if done, e := sel.Terminal(c); done || !errors.Is(e, store.ErrRunNotFound) {
		t.Fatal(done, e)
	}
	calls := 0
	for _, e := range []error{sel.BackfillEvents(c, 0, false, 1, func([]*store.Event) bool { calls++; return true }), sel.BackfillLogs(c, 0, func(int64, []byte) bool { calls++; return true })} {
		if !errors.Is(e, store.ErrRunNotFound) {
			t.Fatal(e)
		}
	}
	if calls != 0 {
		t.Fatal(calls)
	}
}
func TestStreamSelectionHistoricalEmptyBirth(t *testing.T) {
	s, c := valueFormatMongo(t, true)
	r, _ := streamSelectionTestCreate(t, s, c, 1, 0)
	if _, e := s.runs.UpdateOne(c, bson.M{"_id": r.ID}, bson.M{"$unset": bson.M{"birth_id": ""}}); e != nil {
		t.Fatal(e)
	}
	sel, e := s.ResolveStreamSelection(c, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if !sel.present {
		t.Fatal("empty historical birth treated as absence")
	}
	if e = sel.Validate(c); e != nil {
		t.Fatal(e)
	}
	if _, e = s.runs.UpdateOne(c, bson.M{"_id": r.ID}, bson.M{"$set": bson.M{"workflow_name": "old-writer-replacement"}}); e != nil {
		t.Fatal(e)
	}
	if e = sel.Validate(c); e != nil {
		t.Fatalf("nondiscriminating empty-birth compatibility %v", e)
	}
	if _, e = s.runs.DeleteOne(c, bson.M{"_id": r.ID}); e != nil {
		t.Fatal(e)
	}
	streamSelectionTestCreate(t, s, c, 1, 0)
	if e = sel.Validate(c); !errors.Is(e, store.ErrRunDataIdentityChanged) {
		t.Fatalf("qualified successor %v", e)
	}
}
func TestStreamSelectionEventCursorBounds(t *testing.T) {
	s, c := valueFormatMongo(t, true)
	r, b := streamSelectionTestCreate(t, s, c, 1, 0)
	sel, e := s.ResolveStreamSelection(b, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	for _, seq := range []int64{0, math.MaxInt64 - 1, math.MaxInt64} {
		if _, e = sel.EventsCollection().InsertOne(c, bson.M{"run_id": r.ID, "tenant_id": "format-tenant", "seq": seq}); e != nil {
			t.Fatal(e)
		}
	}
	for _, tc := range []struct {
		from  int64
		after bool
		want  int
	}{{0, false, 3}, {0, true, 2}, {math.MaxInt64, false, 1}, {math.MaxInt64, true, 0}} {
		calls := 0
		e = sel.BackfillEvents(c, tc.from, tc.after, 1, func(rows []*store.Event) bool { calls += len(rows); return calls < 5 })
		if e != nil || calls != tc.want {
			t.Fatalf("from%d after%v count%d want%d err%v", tc.from, tc.after, calls, tc.want, e)
		}
	}
}
func TestStreamSelectionCorruptEventTail(t *testing.T) {
	s, c := valueFormatMongo(t, true)
	r, b := streamSelectionTestCreate(t, s, c, 1, 0)
	sel, e := s.ResolveStreamSelection(b, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = sel.EventsCollection().InsertOne(c, bson.M{"run_id": r.ID, "tenant_id": "format-tenant", "seq": "bad"}); e != nil {
		t.Fatal(e)
	}
	calls := 0
	e = sel.BackfillEvents(c, 0, false, 1, func([]*store.Event) bool { calls++; return true })
	if !errors.Is(e, store.ErrEventsCorrupted) || calls != 0 {
		t.Fatalf("event tail decode calls%d err%T %v", calls, e, e)
	}
}
func streamSelectionTestReader(t *testing.T, s *Store, app string, monitor *event.CommandMonitor) *Store {
	t.Helper()
	client, e := mongo.Connect(options.Client().ApplyURI(os.Getenv("ITERION_TEST_MONGO_URI")).SetRegistry(Registry()).SetAppName(app).SetMonitor(monitor))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	db := client.Database(s.db.Name())
	return &Store{client: client, db: db, runs: db.Collection(colRuns), events: db.Collection(colEvents), runLogs: db.Collection(colRunLogs), runSeq: db.Collection(colRunSeq)}
}
func TestStreamSelectionQueryErrorsAndRetry(t *testing.T) {
	s, c := valueFormatMongo(t, true)
	s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
	r, b := streamSelectionTestCreate(t, s, c, 2, 1)
	for i := 0; i < 3; i++ {
		if _, e := s.AppendEvent(b, r.ID, store.Event{}); e != nil {
			t.Fatal(e)
		}
	}
	app := "selection-retry-" + bsonNonce(t)
	var failures atomic.Int64
	reader := streamSelectionTestReader(t, s, app, &event.CommandMonitor{Failed: func(_ context.Context, e *event.CommandFailedEvent) {
		if e.CommandName == "find" {
			failures.Add(1)
		}
	}})
	sel, e := reader.ResolveStreamSelection(c, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	set := func(code int, labels bson.A) {
		t.Helper()
		cmd := bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: bson.M{"times": 1}}, {Key: "data", Value: bson.M{"appName": app, "failCommands": bson.A{"find"}, "errorCode": code, "errorLabels": labels}}}
		if e := s.client.Database("admin").RunCommand(c, cmd).Err(); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() {
		_ = s.client.Database("admin").RunCommand(context.Background(), bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}}).Err()
	})
	set(2, bson.A{})
	calls := 0
	e = sel.BackfillEvents(c, 0, false, 1, func([]*store.Event) bool { calls++; return true })
	var ce mongo.CommandError
	if !errors.As(e, &ce) || ce.Code != 2 || errors.Is(e, store.ErrEventsCorrupted) || calls != 0 || failures.Load() != 1 {
		t.Fatalf("query error changed calls%d failures%d err%T %v", calls, failures.Load(), e, e)
	}
	failures.Store(0)
	var seqs []int64
	e = sel.BackfillEvents(c, 0, false, 1, func(rows []*store.Event) bool {
		seqs = append(seqs, rows[0].Seq)
		if len(seqs) == 1 {
			set(112, bson.A{"TransientTransactionError"})
		}
		return true
	})
	if e != nil || fmt.Sprint(seqs) != "[0 1 2]" || failures.Load() != 1 {
		t.Fatalf("retry callbacks%v failures%d err%v", seqs, failures.Load(), e)
	}
}
func TestStreamSelectionLogSnapshot(t *testing.T) {
	s, c := valueFormatMongo(t, true)
	s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
	r, b := streamSelectionTestCreate(t, s, c, 1, 0)
	if e := s.AppendRunLog(b, r.ID, 0, []byte("AAAAAA")); e != nil {
		t.Fatal(e)
	}
	var armed atomic.Bool
	var finds atomic.Int64
	var mutationErr error
	reader := streamSelectionTestReader(t, s, "selection-snapshot-"+bsonNonce(t), &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if !armed.Load() || e.CommandName != "find" || e.Command.Lookup("find").StringValue() != colRunLogs {
			return
		}
		if finds.Add(1) != 2 {
			return
		}
		armed.Store(false)
		if _, mutationErr = s.runs.DeleteOne(c, bson.M{"_id": r.ID}); mutationErr != nil {
			return
		}
		if _, mutationErr = s.runLogs.DeleteMany(c, bson.M{"run_id": r.ID}); mutationErr != nil {
			return
		}
		_, cb := streamSelectionTestCreate(t, s, c, 1, 0)
		mutationErr = s.AppendRunLog(cb, r.ID, 0, []byte("BBBBBB"))
	}})
	sel, e := reader.ResolveStreamSelection(c, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	armed.Store(true)
	got := ""
	e = sel.BackfillLogs(c, 1, func(_ int64, data []byte) bool { got += string(data); return true })
	if mutationErr != nil {
		t.Fatal(mutationErr)
	}
	if finds.Load() != 2 || got != "AAAAAA" || !errors.Is(e, store.ErrRunDataIdentityChanged) {
		t.Fatalf("snapshot find%d got%q err%v", finds.Load(), got, e)
	}
}
