package mongoutil

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// FindAllSorted runs coll.Find(filter) sorted ascending by sortKey,
// decodes every matching document into a []T, and wraps the two
// possible failures (query, decode) with the caller-supplied messages.
// It is the shared shape behind the many "list X by Y" methods across
// iterion's Mongo-backed stores.
func FindAllSorted[T any](ctx context.Context, coll *mongo.Collection, filter bson.M, sortKey string, findErrMsg, decodeErrMsg string) ([]T, error) {
	cur, err := coll.Find(ctx, filter, options.Find().SetSort(bson.M{sortKey: 1}))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", findErrMsg, err)
	}
	defer cur.Close(ctx)
	var out []T
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", decodeErrMsg, err)
	}
	return out, nil
}
