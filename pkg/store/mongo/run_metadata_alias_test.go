package mongo

import (
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
)

func TestRunMetadataFormatAliasesRefusedBeforeMutation(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	tenant, _ := store.TenantFromContext(ctx)
	for i, key := range []string{"FORMAT_VERSION", "Format_Version", "format_verſion", "FORMAT_VERſION", "format_versİon", "FORMAT_VERSİON"} {
		for _, duplicate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/duplicate=%t", key, duplicate), func(t *testing.T) {
				id := fmt.Sprintf("alias-%d-%t", i, duplicate)
				doc := bson.D{{Key: "_id", Value: id}, {Key: "tenant_id", Value: tenant}, {Key: "version", Value: 1}}
				if duplicate {
					doc = append(doc, bson.E{Key: "format_version", Value: int32(1)})
				}
				doc = append(doc, bson.E{Key: key, Value: int32(2)})
				if _, e := s.runs.InsertOne(ctx, doc); e != nil {
					t.Fatal(e)
				}
				if p, _, e := s.readRunFormatMetadata(ctx, id); !errors.Is(e, store.ErrInvalidValueFormat) {
					t.Errorf("projection accepted alias: parent=%+v err=%v", p, e)
				}
				for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
					filter := bson.M{"_id": id, "$and": bson.A{canonicalValueFormatPredicate(f)}}
					res, e := s.runs.UpdateOne(ctx, filter, bson.M{"$set": bson.M{"test_mutated": true}})
					if e != nil {
						t.Fatal(e)
					}
					if res.MatchedCount != 0 {
						t.Errorf("atomic predicate matched alias for format%d", f)
					}
				}
			})
		}
	}
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		id := fmt.Sprintf("canonical-%d", f)
		if _, e := s.runs.InsertOne(ctx, bson.M{"_id": id, "tenant_id": tenant, "format_version": int32(f), "format_version_extra": 99}); e != nil {
			t.Fatal(e)
		}
		p, _, e := s.readRunFormatMetadata(ctx, id)
		if e != nil || p.FormatVersion != int(f) {
			t.Fatalf("canonical refused %v", e)
		}
		res, e := s.runs.UpdateOne(ctx, bson.M{"_id": id, "$and": bson.A{canonicalValueFormatPredicate(f)}}, bson.M{"$set": bson.M{"ok": true}})
		if e != nil || res.MatchedCount != 1 {
			t.Fatalf("canonical mutation refused %v", e)
		}
	}
}
