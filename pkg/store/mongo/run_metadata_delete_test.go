package mongo

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRunMetadataMongoDeletionHeaders(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"delete", "delete-interrupted", "prune"} {
		for _, tt := range []struct {
			name    string
			headers []any
			want    error
		}{{"known", []any{int32(2)}, nil}, {"future", []any{int32(99)}, store.ErrUnsupportedValueFormat}, {"duplicate", []any{int32(2), int32(99)}, store.ErrInvalidValueFormat}} {
			t.Run(op+"/"+tt.name, func(t *testing.T) {
				s, ctx := valueFormatMongo(t, true)
				var deleted any
				if op != "delete" {
					deleted = time.Now().UTC().Truncate(time.Millisecond)
				}
				doc := bson.D{{Key: "_id", Value: "r"}, {Key: "version", Value: int64(1)}, {Key: "tenant_id", Value: "format-tenant"}, {Key: "status", Value: "running"}, {Key: valueCleanupPendingField, Value: op == "delete-interrupted"}}
				if op == "delete" {
					doc = append(doc, bson.E{Key: "launch_model_overrides", Value: bson.A{bson.M{"selector": "*", "model": "test/model"}}})
				}
				if deleted != nil || tt.name == "duplicate" {
					doc = append(doc, bson.E{Key: "deleted_at", Value: deleted})
				}
				for _, header := range tt.headers {
					doc = append(doc, bson.E{Key: "format_version", Value: header})
				}
				if _, e := s.runs.InsertOne(ctx, doc); e != nil {
					t.Fatal(e)
				}
				before, e := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
				if e != nil {
					t.Fatal(e)
				}
				if op == "prune" {
					var n int
					n, e = s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour))
					if (tt.want == nil && n != 1) || (tt.want != nil && n != 0) {
						t.Fatal(n, e)
					}
				} else {
					e = s.DeleteRun(ctx, "r")
				}
				if tt.want != nil {
					if !errors.Is(e, tt.want) {
						t.Fatalf("err=%v want=%v", e, tt.want)
					}
					after, err := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
					if err != nil || !bytes.Equal(before, after) {
						t.Fatal("refusal changed run", err)
					}
				} else if e != nil {
					t.Fatal(e)
				} else if op == "delete" {
					after, err := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := after.LookupErr("launch_model_overrides"); err == nil {
						t.Fatal("tombstone retained launch routing metadata")
					}
				}
			})
		}
	}
}
