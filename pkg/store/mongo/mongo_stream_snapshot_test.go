package mongo

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestMongoStreamSnapshotRetainsOriginalBirth(t *testing.T) {
	if e := store.RequireRunDataStorage(store.IncarnationRunData); errors.Is(e, store.ErrRunDataCapability) {
		t.Skip("incarnation storage admission is closed")
	} else if e != nil {
		t.Fatal(e)
	}
	for _, f := range []store.ValueFormat{1, 2} {
		for _, mode := range []string{"range", "size"} {
			t.Run(fmt.Sprintf("%d/%s", f, mode), func(t *testing.T) {
				s, ctx := valueFormatMongo(t, true)
				s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
				if e := s.EnsureSchema(ctx, 0); e != nil {
					t.Fatal(e)
				}
				create := func() (*store.Run, context.Context) {
					r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "same", WorkflowName: "wf", Format: f, DataProtocol: store.IncarnationRunData})
					if e != nil {
						t.Fatal(e)
					}
					b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: f, Protocol: r.DataProtocol, BirthID: r.BirthID})
					if e != nil {
						t.Fatal(e)
					}
					c, e := store.WithRunDataBinding(ctx, b)
					if e != nil {
						t.Fatal(e)
					}
					return r, c
				}
				a, ca := create()
				for i, chunk := range []string{"ABCDE", "FGHIJ"} {
					if e := s.AppendRunLog(ca, a.ID, int64(i*5), []byte(chunk)); e != nil {
						t.Fatal(e)
					}
				}
				var fired atomic.Bool
				var aggregates, finds atomic.Int32
				var successor *store.Run
				var successorCtx context.Context
				monitor := &event.CommandMonitor{Started: func(c context.Context, e *event.CommandStartedEvent) {
					if e.CommandName == "find" {
						finds.Add(1)
						if !fired.Load() {
							t.Error("dependent find before replacement barrier")
						}
					}
				}, Succeeded: func(c context.Context, e *event.CommandSucceededEvent) {
					if e.CommandName != "aggregate" {
						return
					}
					aggregates.Add(1)
					if !fired.CompareAndSwap(false, true) {
						return
					}
					if e := s.DeleteRun(ca, a.ID); e != nil {
						t.Fatal(e)
					}
					if n, e := s.PruneDeletionMarkers(ca, time.Now().Add(time.Hour)); e != nil || n != 1 {
						t.Fatalf("prune %d %v", n, e)
					}
					successor, successorCtx = create()
					if successor.BirthID == a.BirthID {
						t.Fatal("same birth")
					}
					if e := s.AppendRunLog(successorCtx, a.ID, 0, []byte("B-successor-longer")); e != nil {
						t.Fatal(e)
					}
				}}
				client, e := mongo.Connect(options.Client().ApplyURI(os.Getenv("ITERION_TEST_MONGO_URI")).SetRegistry(Registry()).SetMonitor(monitor))
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() {
					c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if e := client.Disconnect(c); e != nil {
						t.Error(e)
					}
				})
				db := client.Database(s.db.Name())
				reader := &Store{client: client, db: db, runs: db.Collection(colRuns), runLogs: db.Collection(colRunLogs), events: db.Collection(colEvents), runSeq: db.Collection(colRunSeq), runPlans: db.Collection(colRunPlans)}
				if mode == "range" {
					got, e := reader.ReadRunLogRange(ca, a.ID, 2, 9)
					if e != nil || string(got) != "CDEFGHI" {
						t.Errorf("authorized A snapshot range got=%q err=%v want CDEFGHI", got, e)
					}
				} else {
					got, e := reader.RunLogSize(ca, a.ID)
					if e != nil || got != 10 {
						t.Errorf("authorized A snapshot size got=%d err=%v want10", got, e)
					}
				}
				if !fired.Load() || aggregates.Load() != 1 {
					t.Fatalf("barrier not exactly once: fired=%v aggregates=%d", fired.Load(), aggregates.Load())
				}
				if mode == "range" && finds.Load() != 2 {
					t.Fatalf("anchor/find actual count=%d", finds.Load())
				}
				if mode == "size" && finds.Load() != 1 {
					t.Fatalf("size actual count=%d", finds.Load())
				}
				got, e := s.ReadRunLogRange(successorCtx, a.ID, 0, 0)
				if e != nil || string(got) != "B-successor-longer" {
					t.Fatalf("B changed: %q %v", got, e)
				}
				t.Logf("metadata aggregate=%d dependent finds=%d replacement public birth %s -> %s", aggregates.Load(), finds.Load(), a.BirthID, successor.BirthID)
			})
		}
	}
}
