package mongo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestMongoNotesNamespaceLifecycle(t *testing.T) {
	for _, format := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			requireStreamProtocol(t, store.IncarnationRunData)
			s, ctx := valueFormatMongo(t, true)
			s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
			create := func() (context.Context, *store.Run) {
				t.Helper()
				r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "notes", WorkflowName: "wf", Format: format, DataProtocol: store.IncarnationRunData})
				if e != nil {
					t.Fatal(e)
				}
				c, e := store.BindRunValueFormat(ctx, r)
				if e != nil {
					t.Fatal(e)
				}
				b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: format, Protocol: r.DataProtocol, BirthID: r.BirthID})
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
			if _, e = s.runNotes.InsertOne(ctx, runNoteDoc{TenantID: "format-tenant", RunID: a.ID, Seq: 42, Author: "Alice", Body: "legacy"}); e != nil {
				t.Fatal(e)
			}
			for _, row := range []bson.M{{"_id": bson.D{{Key: "tenant_id", Value: "format-tenant"}, {Key: "run_id", Value: a.ID}}, noteSeqField: int64(100), "next_seq": int64(77), planSeqField: int64(88)}, {"_id": bson.D{{Key: "run_id", Value: a.ID}, {Key: "tenant_id", Value: "format-tenant"}}, noteSeqField: int64(200)}} {
				if _, e = s.runSeq.InsertOne(ctx, row); e != nil {
					t.Fatal(e)
				}
			}
			note, e := s.AppendRunNote(ca, a.ID, store.RunNote{Author: "Alice", Body: "A0"})
			if e != nil || note.Seq != 0 {
				t.Fatalf("strict orphan independence %+v %v", note, e)
			}
			evt, e := s.AppendEvent(ca, a.ID, store.Event{})
			if e != nil || evt.Seq != 0 {
				t.Fatalf("event %+v %v", evt, e)
			}
			plan, inserted, e := s.AppendPlanSnapshot(ca, a.ID, store.PlanSnapshot{Todos: []store.PlanTodo{{Content: "p"}}})
			if e != nil || !inserted || plan.Seq != 0 {
				t.Fatalf("plan %+v %v %v", plan, inserted, e)
			}
			var counters bson.M
			if e = na.seq.FindOne(ctx, bson.M{"_id": na.counterKey(ctx)}).Decode(&counters); e != nil {
				t.Fatal(e)
			}
			for _, field := range []string{"next_seq", planSeqField, noteSeqField} {
				if counters[field] != int64(1) {
					t.Fatalf("independent %s: %v", field, counters)
				}
			}
			session, e := s.client.StartSession()
			if e != nil {
				t.Fatal(e)
			}
			abort := errors.New("abort note allocation")
			_, e = session.WithTransaction(ca, func(sc context.Context) (any, error) {
				if _, e := s.allocStreamSeqField(sc, na, noteSeqField); e != nil {
					return nil, e
				}
				return nil, abort
			})
			session.EndSession(ca)
			if !errors.Is(e, abort) {
				t.Fatal(e)
			}
			note, e = s.AppendRunNote(ca, a.ID, store.RunNote{Author: "Alice", Body: "A1"})
			if e != nil || note.Seq != 1 {
				t.Fatalf("rollback %+v %v", note, e)
			}
			if _, e = na.seq.DeleteOne(ctx, bson.M{"_id": na.counterKey(ctx)}); e != nil {
				t.Fatal(e)
			}
			note, e = s.AppendRunNote(ca, a.ID, store.RunNote{Author: "Alice", Body: "A2"})
			if e != nil || note.Seq != 2 {
				t.Fatalf("tail heal %+v %v", note, e)
			}
			privileged := store.WithoutTenantFilter(t.Context())
			binding, _ := store.RunDataBindingFrom(ca)
			privileged, e = store.WithRunDataBinding(privileged, binding)
			if e != nil {
				t.Fatal(e)
			}
			notes, e := s.ListRunNotes(privileged, a.ID)
			if e != nil || len(notes) != 3 {
				t.Fatalf("privileged %+v %v", notes, e)
			}
			if _, e = s.runs.DeleteOne(ctx, bson.M{"_id": a.ID}); e != nil {
				t.Fatal(e)
			}
			cb, b := create()
			nb, e := s.streamNamespace(cb, b.ID, b)
			if e != nil {
				t.Fatal(e)
			}
			note, e = s.AppendRunNote(cb, b.ID, store.RunNote{Author: "Alice", Body: "B0"})
			if e != nil || note.Seq != 0 {
				t.Fatalf("successor %+v %v", note, e)
			}
			notes, e = s.ListRunNotes(ca, a.ID)
			if !errors.Is(e, store.ErrRunDataIdentityChanged) || notes != nil {
				t.Fatalf("stale notes %+v %v", notes, e)
			}
			notes, e = s.ListRunNotes(cb, b.ID)
			if e != nil || len(notes) != 1 || notes[0].Body != "B0" {
				t.Fatalf("B notes %+v %v", notes, e)
			}
			if e = s.DeleteRun(cb, b.ID); e != nil {
				t.Fatal(e)
			}
			for _, check := range []struct {
				coll   *mongo.Collection
				filter bson.M
				want   int64
			}{{na.notes, na.filter(ctx, nil), 3}, {nb.notes, nb.filter(ctx, nil), 0}, {s.runNotes, bson.M{"run_id": a.ID}, 1}, {s.runSeq, bson.M{"_id.run_id": a.ID}, 2}, {nb.seq, bson.M{"_id": nb.counterKey(ctx)}, 0}} {
				n, e := check.coll.CountDocuments(ctx, check.filter)
				if e != nil || n != check.want {
					t.Fatalf("cleanup %s got%d want%d %v", check.coll.Name(), n, check.want, e)
				}
			}
			if n, e := s.PruneDeletionMarkers(cb, time.Now().Add(time.Hour)); e != nil || n != 1 {
				t.Fatalf("prune %d %v", n, e)
			}
			cc, c := create()
			note, e = s.AppendRunNote(cc, c.ID, store.RunNote{Author: "Alice", Body: "C0"})
			if e != nil || note.Seq != 0 {
				t.Fatalf("public delete recreate %+v %v", note, e)
			}
			nc, e := s.streamNamespace(cc, c.ID, c)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = nc.seq.UpdateOne(ctx, bson.M{"_id": nc.counterKey(ctx)}, bson.M{"$set": bson.M{noteSeqField: int64(math.MaxInt64)}}); e != nil {
				t.Fatal(e)
			}
			if _, e = s.AppendRunNote(cc, c.ID, store.RunNote{Body: "overflow"}); e == nil {
				t.Fatal("overflow accepted")
			}
			// Retired A, current C and legacy orphan all participate in global erasure.
			n, e := s.ReattributeRunNotes(store.WithIdentity(t.Context(), "unrelated-tenant", "operator"), " ALICE ", "uid")
			if e != nil || n != 5 {
				t.Fatalf("global retired/legacy notes %d %v", n, e)
			}
			n, e = s.ReattributeRunNotes(t.Context(), "alice", "uid")
			if e != nil || n != 0 {
				t.Fatalf("idempotent reattribute %d %v", n, e)
			}
		})
	}
}

func TestMongoNotesOpaqueCompatibilityAndRetention(t *testing.T) {
	for _, format := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			s, ctx := valueFormatMongo(t, true)
			c, e := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "orphan", Format: format})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.runNotes.InsertOne(ctx, runNoteDoc{TenantID: "format-tenant", RunID: "orphan", Body: "opaque", Seq: 0}); e != nil {
				t.Fatal(e)
			}
			notes, e := s.ListRunNotes(c, "orphan")
			if e != nil || len(notes) != 1 || notes[0].Body != "opaque" {
				t.Fatalf("orphan %+v %v", notes, e)
			}
			wrong := store.WithIdentity(t.Context(), "other", "user")
			notes, e = s.ListRunNotes(wrong, "orphan")
			if e != nil || len(notes) != 0 {
				t.Fatalf("tenant %+v %v", notes, e)
			}
			canceled, cancel := context.WithCancel(c)
			cancel()
			if _, e = s.ListRunNotes(canceled, "orphan"); !errors.Is(e, context.Canceled) {
				t.Fatalf("cancel %v", e)
			}
			if _, e = s.ListRunNotes(c, "wrong-id"); e == nil {
				t.Fatal("wrong run accepted")
			}
			r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "deleted", Format: format})
			if e != nil {
				t.Fatal(e)
			}
			bound, e := store.BindRunValueFormat(ctx, r)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.AppendRunNote(bound, r.ID, store.RunNote{Body: "to delete"}); e != nil {
				t.Fatal(e)
			}
			if e = s.DeleteRun(bound, r.ID); e != nil {
				t.Fatal(e)
			}
			notes, e = s.ListRunNotes(bound, r.ID)
			if e != nil || len(notes) != 0 {
				t.Fatalf("legacy tombstone %+v %v", notes, e)
			}
			// Index declarations are admission-independent; notes must never inherit TTL.
			for i := 0; i < 2; i++ {
				if e = s.EnsureSchema(ctx, 7); e != nil {
					t.Fatal(e)
				}
			}
			for _, coll := range []*mongo.Collection{s.runNotes, s.db.Collection(colIncarnationNotes)} {
				cur, e := coll.Indexes().List(ctx)
				if e != nil {
					t.Fatal(e)
				}
				var indexes []bson.M
				e = cur.All(ctx, &indexes)
				cur.Close(ctx)
				if e != nil {
					t.Fatal(e)
				}
				if len(indexes) < 3 {
					t.Fatalf("missing note indexes %v", indexes)
				}
				for _, index := range indexes {
					if _, ok := index["expireAfterSeconds"]; ok {
						t.Fatalf("notes TTL %s %v", coll.Name(), index)
					}
				}
			}
		})
	}
}
