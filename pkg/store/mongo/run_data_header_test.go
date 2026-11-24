package mongo

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestObserveRunDataHeaderMongoProjectsOnlyMetadata(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	tenant, _ := store.TenantFromContext(ctx)
	doc := bson.D{{Key: "_id", Value: "opaque"}, {Key: "tenant_id", Value: tenant}, {Key: "birth_id", Value: "birth"}, {Key: "format_version", Value: 1}, {Key: "data_protocol", Value: 1}, {Key: "inputs", Value: bson.A{"not-a-map"}}}
	if _, err := s.runs.InsertOne(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadRun(ctx, "opaque"); err == nil {
		t.Fatal("fixture must fail payload decoding")
	}
	before, err := s.runs.FindOne(ctx, bson.M{"_id": "opaque"}).Raw()
	if err != nil {
		t.Fatal(err)
	}
	h, err := store.ObserveRunDataHeader(ctx, s, "opaque")
	if err != nil || h.RunID != "opaque" || h.BirthID != "birth" || h.Protocol != store.IncarnationRunData {
		t.Fatalf("header=%+v error=%v", h, err)
	}
	after, err := s.runs.FindOne(ctx, bson.M{"_id": "opaque"}).Raw()
	if err != nil || string(after) != string(before) {
		t.Fatalf("observation rewrote row: %v", err)
	}
	foreign := store.WithIdentity(ctx, "another-tenant", "tester")
	if _, err := store.ObserveRunDataHeader(foreign, s, "opaque"); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("foreign header exposed: %v", err)
	}
}

func TestObserveRunDataHeaderMongoRetainsOriginalBirth(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	if _, err := s.CreateRun(ctx, "run", "wf", nil); err != nil {
		t.Fatal(err)
	}
	h, err := store.ObserveRunDataHeader(ctx, s, "run")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.WithRunDataBinding(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(ctx, "run"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune %d: %v", n, err)
	}
	successor, err := s.CreateRun(ctx, "run", "replacement", nil)
	if err != nil {
		t.Fatal(err)
	}
	if successor.BirthID == h.BirthID {
		t.Fatal("fixture reused identity")
	}
	if _, err := store.ObserveRunDataHeader(bound, s, "run"); !errors.Is(err, store.ErrRunDataIdentityChanged) {
		t.Fatalf("adopted successor: %v", err)
	}
}

func TestObserveRunDataHeaderMongoPreservesSelectorAliases(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	tenant, _ := store.TenantFromContext(ctx)
	for i, key := range []string{"_ID", "_İD", "BIRTH_ID", "BİRTH_İD", "DATA_PROTOCOL", "FORMAT_VERSION", "format_verſion", "format_versİon", "V", "DELETED_AT"} {
		t.Run(key, func(t *testing.T) {
			id := fmt.Sprintf("alias-%d", i)
			doc := bson.D{{Key: "_id", Value: id}, {Key: "tenant_id", Value: tenant}, {Key: "format_version", Value: 1}, {Key: "birth_id", Value: "b"}, {Key: "data_protocol", Value: 0}, {Key: key, Value: "alias"}}
			if _, err := s.runs.InsertOne(ctx, doc); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ObserveRunDataHeader(ctx, s, id); !errors.Is(err, store.ErrInvalidRunDataIdentity) {
				t.Fatalf("alias was hidden: %v", err)
			}
		})
	}
}

func TestObserveRunDataHeaderMongoRefusesFutureAndDeleted(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{"future", "v", SchemaVersion + 1},
		{"deleted", "deleted_at", time.Now()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.CreateRun(ctx, tc.name, "wf", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": tc.name}, bson.M{"$set": bson.M{tc.field: tc.value}}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ObserveRunDataHeader(ctx, s, tc.name); err == nil {
				t.Fatal("accepted unavailable run header")
			}
		})
	}
}
