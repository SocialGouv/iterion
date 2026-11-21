package mongo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"sync/atomic"
	"testing"
	"time"
)

func producerBirth(t *testing.T, s *Store, c context.Context, f store.ValueFormat, p store.RunDataProtocol) (*store.Run, context.Context) {
	t.Helper()
	if e := store.RequireRunDataStorage(p); e != nil {
		if p == 1 && errors.Is(e, store.ErrRunDataCapability) {
			t.Skip("admission OFF")
		}
		t.Fatal(e)
	}
	r, e := s.CreateRunWithValueFormat(c, store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: f, DataProtocol: p})
	if e != nil {
		t.Fatal(e)
	}
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: f, Protocol: p, BirthID: r.BirthID})
	if e != nil {
		t.Fatal(e)
	}
	bound, e := store.WithRunDataBinding(c, b)
	if e != nil {
		t.Fatal(e)
	}
	bound, e = store.WithRunValueFormat(bound, store.RunValueFormat{RunID: r.ID, Format: f})
	if e != nil {
		t.Fatal(e)
	}
	return r, bound
}
func TestMongoAppendObservedParentAndProjection(t *testing.T) {
	for _, p := range []store.RunDataProtocol{0, 1} {
		for _, f := range []store.ValueFormat{1, 2} {
			t.Run(fmt.Sprintf("p%d-f%d", p, f), func(t *testing.T) {
				s, c := valueFormatMongo(t, true)
				s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
				r, b := producerBirth(t, s, c, f, p)
				calls := 0
				projected, e := store.WithEventProjection(b, func(_ context.Context, o store.RunObservation) (int64, int64, error) {
					calls++
					if !o.ParentPresent() || o.Header().BirthID != r.BirthID || o.TenantID() != r.TenantID {
						t.Fatal(o)
					}
					return 4, 9, nil
				})
				if e != nil {
					t.Fatal(e)
				}
				stale, _ := store.CaptureRunObservation(store.RunDataHeader{RunID: r.ID, Format: 1, BirthID: "stale"}, "wrong", true)
				in, _ := store.WithEventObservation(store.Event{RunID: r.ID}, stale)
				if f == 2 {
					in.Data = map[string]any{"huge": json.Number("1e400")}
				}
				got, e := store.AppendObservedEvent(projected, s, r.ID, in)
				if e != nil {
					t.Fatal(e)
				}
				o, ok := got.EventObservation()
				if !ok || o.Header().BirthID != r.BirthID || o.Header().Format != f || got.LogOffset != 4 || got.ActiveMs != 9 || calls != 1 {
					t.Fatal(got, o, calls)
				}
				// Admin capture uses the selected parent's canonical tenant, not empty caller identity.
				admin := store.WithoutTenantFilter(t.Context())
				binding, _ := store.CaptureRunDataBinding(o.Header())
				admin, e = store.WithRunDataBinding(admin, binding)
				if e != nil {
					t.Fatal(e)
				}
				admin, e = store.WithRunValueFormat(admin, store.RunValueFormat{RunID: r.ID, Format: f})
				if e != nil {
					t.Fatal(e)
				}
				privileged, e := s.AppendEvent(admin, r.ID, store.Event{})
				if e != nil {
					t.Fatal(e)
				}
				po, ok := privileged.EventObservation()
				if !ok || po.TenantID() != r.TenantID {
					t.Fatal(po)
				}
			})
		}
	}
}

func TestMongoPreparedObservationRejectsReplacement(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(fmt.Sprint(absent), func(t *testing.T) {
			s, c := valueFormatMongo(t, true)
			s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
			var original *store.Run
			var bound context.Context
			if !absent {
				original, bound = producerBirth(t, s, c, 1, 0)
			}
			calls := 0
			projected, e := store.WithEventProjection(c, func(_ context.Context, o store.RunObservation) (int64, int64, error) {
				calls++
				if o.ParentPresent() == absent {
					return 0, 0, fmt.Errorf("wrong initial presence")
				}
				if !absent {
					if o.Header().BirthID != original.BirthID {
						return 0, 0, fmt.Errorf("wrong original")
					}
					if e := s.DeleteRun(bound, "r"); e != nil {
						return 0, 0, e
					}
					if _, e := s.PruneDeletionMarkers(c, time.Now().Add(time.Hour)); e != nil {
						return 0, 0, e
					}
				}
				_, e := s.CreateRun(c, "r", "replacement", nil)
				return 8, 13, e
			})
			if e != nil {
				t.Fatal(e)
			}
			got, e := s.AppendEvent(projected, "r", store.Event{})
			if got != nil || !errors.Is(e, store.ErrRunDataIdentityChanged) || calls != 1 {
				t.Fatal(got, e, calls)
			}
			n, e := s.events.CountDocuments(c, bson.M{"run_id": "r"})
			if e != nil || n != 0 {
				t.Fatal("publication survived refusal", n, e)
			}
		})
	}
}

func TestMongoObservedProjectionOutsideTransactionRetry(t *testing.T) {
	s, c := valueFormatMongo(t, true)
	r, b := producerBirth(t, s, c, 1, 0)
	app := "producer-retry-" + bsonNonce(t)
	var failures atomic.Int32
	writer := streamSelectionTestReader(t, s, app, &event.CommandMonitor{Failed: func(_ context.Context, e *event.CommandFailedEvent) {
		if e.CommandName == "update" {
			failures.Add(1)
		}
	}})
	calls := 0
	projected, e := store.WithEventProjection(b, func(context.Context, store.RunObservation) (int64, int64, error) {
		calls++
		cmd := bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: bson.M{"times": 1}}, {Key: "data", Value: bson.M{"appName": app, "failCommands": bson.A{"update"}, "errorCode": 112, "errorLabels": bson.A{"TransientTransactionError"}}}}
		e := s.client.Database("admin").RunCommand(c, cmd).Err()
		return 3, 5, e
	})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_ = s.client.Database("admin").RunCommand(context.Background(), bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"}}).Err()
	})
	got, e := store.AppendObservedEvent(projected, writer, r.ID, store.Event{})
	if e != nil {
		t.Fatal(e)
	}
	o, ok := got.EventObservation()
	if !ok || o.Header().BirthID != r.BirthID || calls != 1 || failures.Load() != 1 || got.Seq != 0 || got.LogOffset != 3 || got.ActiveMs != 5 {
		t.Fatal(got, o, calls, failures.Load())
	}
	n, e := s.events.CountDocuments(c, bson.M{"run_id": r.ID})
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
}
