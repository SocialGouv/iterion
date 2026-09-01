package mongoutil

import (
	"context"
	"errors"
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

// FindOne runs coll.FindOne(filter) and decodes the match into a T,
// mapping mongo.ErrNoDocuments to the caller's notFoundErr sentinel and
// wrapping any other failure with errMsg. It is the shared shape behind
// the many "get X [by Y]" methods across iterion's Mongo-backed stores.
func FindOne[T any](ctx context.Context, coll *mongo.Collection, filter bson.M, notFoundErr error, errMsg string) (T, error) {
	var out T
	err := coll.FindOne(ctx, filter).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return out, notFoundErr
	}
	if err != nil {
		return out, fmt.Errorf("%s: %w", errMsg, err)
	}
	return out, nil
}

// FindOneAndDeleteChecked runs coll.FindOneAndDelete(filter) and decodes
// the removed document into a T, mapping mongo.ErrNoDocuments to the
// caller's notFoundErr sentinel and wrapping any other failure with
// errMsg. It is the shared shape behind the single-use "take X" methods
// (get-and-delete) across iterion's Mongo-backed stores.
func FindOneAndDeleteChecked[T any](ctx context.Context, coll *mongo.Collection, filter bson.M, notFoundErr error, errMsg string) (T, error) {
	var out T
	err := coll.FindOneAndDelete(ctx, filter).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return out, notFoundErr
	}
	if err != nil {
		return out, fmt.Errorf("%s: %w", errMsg, err)
	}
	return out, nil
}

// ReplaceOneChecked replaces the document matching filter with doc,
// mapping a duplicate-key conflict to dupErr (pass nil to skip that
// check) and a zero-match result to notFoundErr. It is the shared shape
// behind the many "update X" methods that replace a whole document.
func ReplaceOneChecked(ctx context.Context, coll *mongo.Collection, filter bson.M, doc any, dupErr, notFoundErr error, errMsg string) error {
	res, err := coll.ReplaceOne(ctx, filter, doc)
	if err != nil {
		if dupErr != nil && IsDuplicateKey(err) {
			return dupErr
		}
		return fmt.Errorf("%s: %w", errMsg, err)
	}
	if res.MatchedCount == 0 {
		return notFoundErr
	}
	return nil
}

// UpdateOneChecked applies update (a partial $set/$inc/... document) to
// the document matching filter, mapping a zero-match result to
// notFoundErr. It is the shared shape behind the many "update field(s)
// of X" methods that reject an update to a missing document, as
// opposed to ReplaceOneChecked's whole-document replace.
// update accepts a bson.M operator document or a mongo.Pipeline
// (aggregation-pipeline update) — UpdateOne takes either.
func UpdateOneChecked(ctx context.Context, coll *mongo.Collection, filter bson.M, update any, notFoundErr error, errMsg string) error {
	res, err := coll.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("%s: %w", errMsg, err)
	}
	if res.MatchedCount == 0 {
		return notFoundErr
	}
	return nil
}

// DeleteOneChecked deletes the document matching filter, mapping a
// zero-match result to notFoundErr. It is the shared shape behind the
// many "delete X" methods that treat deleting a missing document as an
// error (as opposed to a silent no-op).
func DeleteOneChecked(ctx context.Context, coll *mongo.Collection, filter bson.M, notFoundErr error, errMsg string) error {
	res, err := coll.DeleteOne(ctx, filter)
	if err != nil {
		return fmt.Errorf("%s: %w", errMsg, err)
	}
	if res.DeletedCount == 0 {
		return notFoundErr
	}
	return nil
}

// SetBodyWithoutID marshals a document into a `$set` body with `_id`
// removed. Mongo rejects an update that touches `_id` ("Mod on _id not
// allowed"), so an upsert that keeps the id in `$setOnInsert` must strip it
// from the `$set` half — a dance every store here performs identically.
func SetBodyWithoutID(v any, what string) (bson.M, error) {
	raw, err := bson.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal: %w", what, err)
	}
	var body bson.M
	if err := bson.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("%s: re-decode: %w", what, err)
	}
	delete(body, "_id")
	return body, nil
}
