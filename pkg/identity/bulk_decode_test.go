package identity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func witnessBulkDecode[T any](t *testing.T, idOf func(T) string) {
	t.Helper()
	ctx := context.Background()
	for _, broken := range []string{"before", "between", "after"} {
		t.Run(broken, func(t *testing.T) {
			docs := []any{bson.M{"_id": "first", "name": "First"}, bson.M{"_id": "last", "name": "Last"}}
			bad := bson.M{"_id": "corrupt", "name": int32(42)}
			switch broken {
			case "before":
				docs = append([]any{bad}, docs...)
			case "between":
				docs = []any{docs[0], bad, docs[1]}
			case "after":
				docs = append(docs, bad)
			}
			cur, err := mongo.NewCursorFromDocuments(docs, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cur.Close(ctx)
			got, err := decodeBulkCursor(ctx, cur, idOf, "decode bulk")
			if len(got) != 2 || idOf(got["first"]) != "first" || idOf(got["last"]) != "last" {
				t.Fatalf("a corrupt document starved its readable siblings: %v (error %v)", got, err)
			}
			partial, ok := err.(*BulkDecodeError)
			if !ok || len(partial.Failures) != 1 || partial.Failures["corrupt"] == nil || !strings.Contains(err.Error(), `document "corrupt"`) {
				t.Fatalf("corruption must name its document in a typed error: %v", err)
			}
		})
	}
	brokenCursor := errors.New("cursor disconnected")
	cur, err := mongo.NewCursorFromDocuments([]any{bson.M{"_id": "good", "name": "Healthy"}, bson.M{"_id": "bad", "name": int32(42)}}, brokenCursor, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close(ctx)
	got, err := decodeBulkCursor(ctx, cur, idOf, "decode bulk")
	if got != nil || !errors.Is(err, brokenCursor) {
		t.Fatalf("cursor failure masquerades as a completed partial read: %v, %v", got, err)
	}
}

func TestBulkDecodeKeepsReadableSiblingsAndNamesCorruption(t *testing.T) {
	t.Run("orgs", func(t *testing.T) { witnessBulkDecode(t, func(o Org) string { return o.ID }) })
	t.Run("teams", func(t *testing.T) { witnessBulkDecode(t, func(o Team) string { return o.ID }) })
	t.Run("users", func(t *testing.T) { witnessBulkDecode(t, func(o User) string { return o.ID }) })
}
