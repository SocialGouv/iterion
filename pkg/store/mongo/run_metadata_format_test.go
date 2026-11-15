package mongo

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func metadataHeaderDocument(id string, headers ...any) bson.D {
	doc := bson.D{{Key: "_id", Value: id}, {Key: "tenant_id", Value: "format-tenant"}, {Key: "status", Value: "queued"}}
	for _, value := range headers {
		doc = append(doc, bson.E{Key: "format_version", Value: value})
	}
	return doc
}

func TestRunMetadataFormatHeaderMatrix(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	cases := []struct {
		name    string
		headers []any
		format  store.ValueFormat
		want    error
	}{
		{"absent", nil, 1, nil}, {"zero", []any{int32(0)}, 1, nil}, {"one", []any{int64(1)}, 1, nil},
		{"two32", []any{int32(2)}, 2, nil}, {"two64", []any{int64(2)}, 2, nil},
		{"future", []any{int64(99)}, 0, store.ErrUnsupportedValueFormat},
		{"negative", []any{int64(-1)}, 0, store.ErrUnsupportedValueFormat},
		{"null", []any{nil}, 0, store.ErrInvalidValueFormat},
		{"array", []any{bson.A{2}}, 0, store.ErrInvalidValueFormat},
		{"double", []any{float64(2)}, 0, store.ErrInvalidValueFormat},
		{"string", []any{"2"}, 0, store.ErrInvalidValueFormat},
		{"duplicate-same", []any{int32(2), int64(2)}, 0, store.ErrInvalidValueFormat},
		{"duplicate-future", []any{int32(2), int64(99)}, 0, store.ErrInvalidValueFormat},
		{"duplicate-projected", []any{int32(2), int64(99)}, 0, store.ErrInvalidValueFormat},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			doc := metadataHeaderDocument(tt.name, tt.headers...)
			if tt.name == "duplicate-projected" {
				doc = metadataHeaderDocument(tt.name)
				doc = append(doc, bson.E{Key: "version", Value: int64(1)}, bson.E{Key: "deleted_at", Value: nil})
				for _, value := range tt.headers {
					doc = append(doc, bson.E{Key: "format_version", Value: value})
				}
			}
			if _, err := s.runs.InsertOne(ctx, doc); err != nil {
				t.Fatal(err)
			}
			before, err := s.runs.FindOne(ctx, bson.M{"_id": tt.name}).Raw()
			if err != nil {
				t.Fatal(err)
			}
			parent, err := s.valueParent(ctx, tt.name)
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("parent=%v error=%v want %v", parent, err, tt.want)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				got, err := store.NormalizeValueFormat(parent.FormatVersion)
				if err != nil || got != tt.format {
					t.Fatal(parent, got, err)
				}
			}
			result, err := s.updateRunMetadata(ctx, tt.name, bson.M{"status": "queued"}, versionRunUpdate(bson.M{"$set": bson.M{"name": "metadata"}}))
			if tt.want != nil {
				if !errors.Is(err, tt.want) || result != nil {
					t.Fatal(result, err)
				}
				after, err := s.runs.FindOne(ctx, bson.M{"_id": tt.name}).Raw()
				if err != nil || string(after) != string(before) {
					t.Fatal("refusal changed document", err)
				}
			} else if err != nil || result.MatchedCount != 1 {
				t.Fatal(result, err)
			}
		})
	}
}

func TestRunMetadataFormatAtomicShape(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	cases := []struct {
		name                  string
		headers               []any
		wantLegacy, wantExact bool
	}{
		{"absent", nil, true, false}, {"zero", []any{int32(0)}, true, false}, {"one", []any{int64(1)}, true, false},
		{"two", []any{int64(2)}, false, true}, {"future", []any{int64(99)}, false, false},
		{"double", []any{float64(2)}, false, false}, {"null", []any{nil}, false, false},
		{"array", []any{bson.A{2}}, false, false}, {"duplicate", []any{int32(2), int64(99)}, false, false},
		{"duplicate-legacy", []any{int32(1), int32(1)}, false, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.runs.InsertOne(ctx, metadataHeaderDocument(tt.name, tt.headers...)); err != nil {
				t.Fatal(err)
			}
			for _, f := range []store.ValueFormat{1, 2, 99} {
				// Exercise the production predicate in the actual mutating command,
				// without relying on the preflight reader to reject the bad header.
				filter := bson.M{"_id": tt.name, "$and": bson.A{canonicalValueFormatPredicate(f)}}
				result, err := s.runs.UpdateOne(ctx, filter, bson.M{"$set": bson.M{fmt.Sprintf("probe_%d", f): true}})
				if err != nil {
					t.Fatal(f, err)
				}
				want := f == 1 && tt.wantLegacy || f == 2 && tt.wantExact
				if (result.MatchedCount == 1) != want {
					t.Fatalf("format %d matched=%d want=%t", f, result.MatchedCount, want)
				}
			}
		})
	}
}

func TestRunMetadataFormatCapturedAndBusinessCAS(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	for _, id := range []string{"changed", "future", "duplicate", "benign", "bound", "other-tenant"} {
		if _, err := s.runs.InsertOne(ctx, metadataHeaderDocument(id, int32(2))); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		id          string
		replacement bson.D
		want        error
	}{
		{"changed", metadataHeaderDocument("changed", int32(1)), store.ErrValueFormatChanged},
		{"future", metadataHeaderDocument("future", int32(99)), store.ErrUnsupportedValueFormat},
		{"duplicate", metadataHeaderDocument("duplicate", int32(2), int32(99)), store.ErrInvalidValueFormat},
	} {
		guard, err := s.prepareRunMetadataFormat(ctx, tt.id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.runs.ReplaceOne(ctx, bson.M{"_id": tt.id}, tt.replacement); err != nil {
			t.Fatal(err)
		}
		result, err := s.runs.UpdateOne(ctx, guard.filter(ctx, bson.M{}), bson.M{"$set": bson.M{"name": "escaped"}})
		if err != nil || result.MatchedCount != 0 {
			t.Fatal(tt.id, result, err)
		}
		if err = guard.checkMiss(ctx, s); !errors.Is(err, tt.want) {
			t.Fatal(tt.id, err)
		}
	}
	guard, err := s.prepareRunMetadataFormat(ctx, "benign")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.runs.UpdateOne(ctx, bson.M{"_id": "benign"}, bson.M{"$set": bson.M{"runner_version": "other"}}); err != nil {
		t.Fatal(err)
	}
	result, err := s.runs.UpdateOne(ctx, guard.filter(ctx, bson.M{"status": "queued"}), versionRunUpdate(bson.M{"$set": bson.M{"name": "ok"}}))
	if err != nil || result.MatchedCount != 1 {
		t.Fatal(result, err)
	}
	result, err = s.updateRunMetadata(ctx, "benign", bson.M{"status": "running"}, bson.M{"$set": bson.M{"name": "bad"}})
	if err != nil || result.MatchedCount != 0 {
		t.Fatal("business CAS changed", result, err)
	}
	bound, err := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "bound", Format: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.updateRunMetadata(bound, "bound", bson.M{}, bson.M{"$set": bson.M{"name": "bad"}}); !errors.Is(err, store.ErrValueFormatChanged) {
		t.Fatal(err)
	}
	foreign := store.WithTenant(ctx, "foreign")
	result, err = s.updateRunMetadata(foreign, "other-tenant", bson.M{}, bson.M{"$set": bson.M{"name": "bad"}})
	if err != nil || result.MatchedCount != 0 {
		t.Fatal("tenant bypass", result, err)
	}
	result, err = s.updateRunMetadata(ctx, "absent", bson.M{}, bson.M{"$set": bson.M{"name": "bad"}})
	if err != nil || result.MatchedCount != 0 {
		t.Fatal(result, err)
	}
	if n, err := s.runs.CountDocuments(ctx, bson.M{"_id": "absent"}); err != nil || n != 0 {
		t.Fatal("metadata created", n, err)
	}
}

func TestRunMetadataFormatFindOptionsAndErrors(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	if _, err := s.runs.InsertOne(ctx, metadataHeaderDocument("r", int32(2))); err != nil {
		t.Fatal(err)
	}
	result, err := s.findOneAndUpdateRunMetadata(ctx, "r", bson.M{"status": "queued"}, bson.M{"$set": bson.M{"name": "ok"}}, options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"name": 1}))
	if err != nil {
		t.Fatal(err)
	}
	var doc bson.M
	if err = result.Decode(&doc); err != nil || doc["name"] != "ok" || doc["status"] != nil {
		t.Fatal(doc, err)
	}
	result, err = s.findOneAndUpdateRunMetadata(ctx, "r", bson.M{"status": "running"}, bson.M{"$set": bson.M{"name": "bad"}})
	if err != nil || !errors.Is(result.Err(), mongo.ErrNoDocuments) {
		t.Fatal(result, err)
	}
	result, err = s.findOneAndUpdateRunMetadata(ctx, "missing", bson.M{}, bson.M{"$set": bson.M{"name": "bad"}}, options.FindOneAndUpdate().SetUpsert(true))
	if !errors.Is(err, store.ErrInvalidValueFormat) || result != nil {
		t.Fatal(result, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.updateRunMetadata(cancelled, "r", bson.M{}, bson.M{"$set": bson.M{"name": "bad"}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, err = s.updateRunMetadata(ctx, "r", bson.M{}, bson.M{"$set": bson.M{"_id": "replacement"}})
	var writeError mongo.WriteException
	if !errors.As(err, &writeError) {
		t.Fatalf("driver error masked: %T %v", err, err)
	}
}

func TestRunMetadataFormatDoesNotDecodePayload(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	doc := metadataHeaderDocument("r", int32(2))
	doc = append(doc, bson.E{Key: "inputs", Value: "deliberately not an exact frame"})
	if _, err := s.runs.InsertOne(ctx, doc); err != nil {
		t.Fatal(err)
	}
	result, err := s.updateRunMetadata(ctx, "r", bson.M{}, bson.M{"$set": bson.M{"runner_version": "metadata-only"}})
	if err != nil || result.MatchedCount != 1 {
		t.Fatal(result, err)
	}
	var after bson.M
	if err = s.runs.FindOne(ctx, bson.M{"_id": "r"}).Decode(&after); err != nil || after["inputs"] != "deliberately not an exact frame" {
		t.Fatal(after, err)
	}
}

func TestRunMetadataFormatLegacySaveBirth(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	r := &store.Run{ID: "legacy-save", Status: store.RunStatusQueued}
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("legacy zero-version SaveRun birth: %v", err)
	}
	loaded, err := s.LoadRun(ctx, r.ID)
	if err != nil || loaded.ID != r.ID || loaded.FormatVersion != 0 {
		t.Fatal(loaded, err)
	}
}
