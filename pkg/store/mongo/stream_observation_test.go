package mongo

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
)

func TestReaderOriginMongoMatrix(t *testing.T) {
	for _, p := range []store.RunDataProtocol{0, 1} {
		for _, f := range []store.ValueFormat{1, 2} {
			t.Run(fmt.Sprintf("p%d-f%d", p, f), func(t *testing.T) {
				s, c := valueFormatMongo(t, true)
				s.blob = &namespaceInboxBlob{&valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}}
				r, b := producerBirth(t, s, c, f, p)
				_, e := s.AppendEvent(b, "r", store.Event{})
				if e != nil {
					t.Fatal(e)
				}
				sel, e := s.ResolveStreamSelection(b, "r")
				if e != nil {
					t.Fatal(e)
				}
				o, ok, e := store.CaptureEventStreamObservation(sel)
				if e != nil || !ok || o.TenantID() != r.TenantID {
					t.Fatal(o, e)
				}
				calls := 0
				if e = sel.BackfillEvents(b, 0, false, 1, func(events []*store.Event) bool {
					calls++
					for _, v := range events {
						got, known := v.EventObservation()
						if !known || got != o {
							t.Fatal(got)
						}
					}
					return true
				}); e != nil || calls != 1 {
					t.Fatal(e, calls)
				}
				// An administrative retained reader still captures stored tenant, not request.
				admin := store.WithoutTenantFilter(t.Context())
				a, e := s.ResolveStreamSelection(admin, "r")
				if e != nil {
					t.Fatal(e)
				}
				if a.EventStreamObservation() != o {
					t.Fatal(a.EventStreamObservation())
				}
				_, e = s.runs.UpdateOne(c, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"tenant_id": "new"}})
				if e != nil {
					t.Fatal(e)
				}
				if e = a.Validate(admin); !errors.Is(e, store.ErrRunDataIdentityChanged) && !errors.Is(e, store.ErrRunNotFound) {
					t.Fatal(e)
				}
			})
		}
	}
}
func TestReaderOriginMongoCanonicalTenant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries bson.D
		valid   bool
	}{{"missing", nil, true}, {"empty", bson.D{{Key: "tenant_id", Value: ""}}, true}, {"string", bson.D{{Key: "tenant_id", Value: "t"}}, true}, {"null", bson.D{{Key: "tenant_id", Value: nil}}, false}, {"number", bson.D{{Key: "tenant_id", Value: 2}}, false}, {"duplicate", bson.D{{Key: "tenant_id", Value: "a"}, {Key: "tenant_id", Value: "b"}}, false}, {"alias", bson.D{{Key: "TENANT_ID", Value: "a"}}, false}, {"fold", bson.D{{Key: "tenant_İd", Value: "a"}}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := valueFormatMongo(t, true)
			c := store.WithoutTenantFilter(t.Context())
			initial := bson.D{{Key: "_id", Value: "r"}, {Key: "status", Value: "running"}}
			if _, e := s.runs.InsertOne(c, initial); e != nil {
				t.Fatal(e)
			}
			sel, e := s.ResolveStreamSelection(c, "r")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.runs.DeleteOne(c, bson.M{"_id": "r"}); e != nil {
				t.Fatal(e)
			}
			raw := append(bson.D{{Key: "_id", Value: "r"}, {Key: "status", Value: "running"}}, tc.entries...)
			if _, e = s.runs.InsertOne(c, raw); e != nil {
				t.Fatal(e)
			}
			_, e = s.ResolveStreamSelection(c, "r")
			if tc.valid {
				if e != nil {
					t.Fatal(e)
				}
			} else if !errors.Is(e, store.ErrInvalidRunDataIdentity) {
				t.Fatal("capture", e)
			}
			e = sel.Validate(c)
			if !tc.valid {
				if !errors.Is(e, store.ErrInvalidRunDataIdentity) {
					t.Fatal("retained", e)
				}
			} else if tc.name == "string" {
				if !errors.Is(e, store.ErrRunDataIdentityChanged) {
					t.Fatal(e)
				}
			} else if e != nil {
				t.Fatal(e)
			}
			// Default metadata policy remains the preexisting decoder; canonical validation is opt-in.
			if tc.name == "alias" || tc.name == "null" || tc.name == "duplicate" {
				if _, _, e = s.readRunFormatMetadata(c, "r"); e != nil {
					t.Fatal("writer policy changed", e)
				}
			}
		})
	}
}

func TestReaderOriginMongoTenantBetweenPages(t *testing.T) {
	s, c := valueFormatMongo(t, true)
	_, b := producerBirth(t, s, c, 1, 0)
	for i := 0; i < 2; i++ {
		if _, e := s.AppendEvent(b, "r", store.Event{}); e != nil {
			t.Fatal(e)
		}
	}
	sel, e := s.ResolveStreamSelection(b, "r")
	if e != nil {
		t.Fatal(e)
	}
	o := sel.EventStreamObservation()
	calls := 0
	e = sel.BackfillEvents(b, 0, false, 1, func(events []*store.Event) bool {
		calls++
		if calls != 1 || len(events) != 1 || events[0].Seq != 0 {
			t.Fatal("second page delivered", calls, events)
		}
		got, ok := events[0].EventObservation()
		if !ok || got != o {
			t.Fatal(got)
		}
		if _, e := s.runs.UpdateOne(c, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"tenant_id": "other"}}); e != nil {
			t.Fatal(e)
		}
		return true
	})
	if calls != 1 || !errors.Is(e, store.ErrRunDataIdentityChanged) {
		t.Fatal(calls, e)
	}
}

type readerMongoInherited struct{ *StreamSelection }

func (r *readerMongoInherited) DecodeEvent(context.Context, bson.Raw) (*store.Event, error) {
	panic("inherited capability bypassed")
}
func TestReaderOriginMongoReceiver(t *testing.T) {
	o, e := store.CaptureRunObservation(store.RunDataHeader{RunID: "r", Format: 1}, "", true)
	if e != nil {
		t.Fatal(e)
	}
	var nilSelection *StreamSelection
	for _, v := range []any{nilSelection, &readerMongoInherited{&StreamSelection{observation: o}}} {
		if _, _, e := store.CaptureEventStreamObservation(v); !errors.Is(e, store.ErrRunDataCapability) {
			t.Fatal(e)
		}
	}
}
