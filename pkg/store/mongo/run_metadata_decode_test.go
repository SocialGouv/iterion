package mongo

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// A run document this binary cannot decode — a future schema version, a
// field of the wrong BSON type — is reported as ErrRunMetadataUndecodable:
// reading it again answers the same.
func TestLoadRunReportsUndecodableMetadata(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	for _, tc := range []struct {
		id  string
		set bson.M
	}{
		{"future-schema", bson.M{"v": SchemaVersion + 1}},
		{"wrong-type", bson.M{"workflow_name": bson.A{"not", "a", "string"}}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if _, err := s.CreateRun(ctx, tc.id, "wf", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := s.runs.UpdateOne(ctx, withTenantFilter(ctx, bson.M{"_id": tc.id}), bson.M{"$set": tc.set}); err != nil {
				t.Fatal(err)
			}
			_, err := s.LoadRun(ctx, tc.id)
			if !errors.Is(err, store.ErrRunMetadataUndecodable) {
				t.Fatalf("LoadRun = %v, want ErrRunMetadataUndecodable", err)
			}
		})
	}
}
