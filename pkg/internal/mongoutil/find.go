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

// NormalizePage clamps a caller-supplied (offset, limit) pair to the
// non-negative range Mongo's SetSkip/SetLimit expect, substituting
// defaultLimit when limit is unset (<= 0).
func NormalizePage(offset, limit int, defaultLimit int64) (skip, take int64) {
	take = int64(limit)
	if take <= 0 {
		take = defaultLimit
	}
	skip = int64(offset)
	if skip < 0 {
		skip = 0
	}
	return skip, take
}

// FindPageSorted is FindAllSorted with Skip/Limit pagination, for the
// "list X" methods that page over an entire collection rather than
// filtering by a parent id.
func FindPageSorted[T any](ctx context.Context, coll *mongo.Collection, filter bson.M, sortKey string, skip, limit int64, findErrMsg, decodeErrMsg string) ([]T, error) {
	cur, err := coll.Find(ctx, filter, options.Find().SetSort(bson.M{sortKey: 1}).SetSkip(skip).SetLimit(limit))
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
