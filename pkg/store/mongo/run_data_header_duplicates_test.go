package mongo

import (
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
)

func TestRunDataHeaderObservationDuplicateSelectors(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	tenant, _ := store.TenantFromContext(ctx)
	for i, k := range []string{"birth_id", "format_version", "data_protocol", "v", "deleted_at"} {
		t.Run(k, func(t *testing.T) {
			var val any = int32(1)
			if k == "birth_id" {
				val = "b"
			}
			if k == "deleted_at" {
				val = nil
			}
			id := fmt.Sprint("duplicate-", i)
			doc := bson.D{{Key: "_id", Value: id}, {Key: "tenant_id", Value: tenant}, {Key: k, Value: val}, {Key: k, Value: val}}
			if _, e := s.runs.InsertOne(ctx, doc); e != nil {
				t.Fatal(e)
			}
			if h, e := store.ObserveRunDataHeader(ctx, s, id); !errors.Is(e, store.ErrInvalidRunDataIdentity) {
				t.Fatalf("duplicate %s accepted: %+v %v", k, h, e)
			}
		})
	}
}
