package storekit

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// Mongo wraps one typed collection with the domain package's error
// vocabulary: notFound is the sentinel misses map to, and prefix is the
// package tag every wrapped failure carries ("pat" → "pat: <op>: …").
// It composes on pkg/internal/mongoutil for the shapes mongoutil already
// has and only adds the ones it lacks (inserts, option-driven finds,
// unchecked updates, delete-many).
type Mongo[T any] struct {
	coll     *mongo.Collection
	notFound error
	prefix   string
}

// NewMongo wires a typed collection wrapper.
func NewMongo[T any](coll *mongo.Collection, notFound error, prefix string) *Mongo[T] {
	return &Mongo[T]{coll: coll, notFound: notFound, prefix: prefix}
}

// Coll exposes the underlying collection for the domain-specific
// operations that stay in the domain package (CAS updates, counters).
func (s *Mongo[T]) Coll() *mongo.Collection { return s.coll }

func (s *Mongo[T]) msg(op string) string { return s.prefix + ": " + op }

// Insert stores doc, mapping a duplicate-key conflict to dup (nil skips
// that mapping) and wrapping any other failure as "<prefix>: <op>".
func (s *Mongo[T]) Insert(ctx context.Context, doc T, dup error, op string) error {
	if _, err := s.coll.InsertOne(ctx, doc); err != nil {
		if dup != nil && mongoutil.IsDuplicateKey(err) {
			return dup
		}
		return fmt.Errorf("%s: %w", s.msg(op), err)
	}
	return nil
}

// GetByID returns the document whose _id is id, or notFound.
func (s *Mongo[T]) GetByID(ctx context.Context, id string, op string) (T, error) {
	return mongoutil.FindOne[T](ctx, s.coll, bson.M{"_id": id}, s.notFound, s.msg(op))
}

// FindOne returns the document matching filter, or notFound — the
// secondary-key lookup shape (token hash, idempotency key).
func (s *Mongo[T]) FindOne(ctx context.Context, filter bson.M, op string) (T, error) {
	return mongoutil.FindOne[T](ctx, s.coll, filter, s.notFound, s.msg(op))
}

// List returns every document matching filter under the given find
// options (sort/skip/limit stay at the call site, where the domain
// ordering lives). findOp and decodeOp name the two failure surfaces.
func (s *Mongo[T]) List(ctx context.Context, filter bson.M, findOp, decodeOp string, opts ...options.Lister[options.FindOptions]) ([]T, error) {
	cur, err := s.coll.Find(ctx, filter, opts...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.msg(findOp), err)
	}
	defer cur.Close(ctx)
	var out []T
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", s.msg(decodeOp), err)
	}
	return out, nil
}

// Replace overwrites the document whose _id is id, mapping a
// duplicate-key conflict to dup (nil skips) and zero matches to notFound.
func (s *Mongo[T]) Replace(ctx context.Context, id string, doc T, dup error, op string) error {
	return mongoutil.ReplaceOneChecked(ctx, s.coll, bson.M{"_id": id}, doc, dup, s.notFound, s.msg(op))
}

// Set applies a $set of fields to the document whose _id is id, mapping
// zero matches to notFound.
func (s *Mongo[T]) Set(ctx context.Context, id string, fields bson.M, op string) error {
	return mongoutil.UpdateOneChecked(ctx, s.coll, bson.M{"_id": id}, bson.M{"$set": fields}, s.notFound, s.msg(op))
}

// SetAny is Set without the zero-match check — for best-effort stamps
// (last_used_at) where a vanished row is not an error.
func (s *Mongo[T]) SetAny(ctx context.Context, id string, fields bson.M, op string) error {
	if _, err := s.coll.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": fields}); err != nil {
		return fmt.Errorf("%s: %w", s.msg(op), err)
	}
	return nil
}

// Delete removes the document whose _id is id, mapping zero matches to
// notFound.
func (s *Mongo[T]) Delete(ctx context.Context, id string, op string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id}, s.notFound, s.msg(op))
}

// DeleteWhere removes every document matching filter; removing nothing
// is not an error.
func (s *Mongo[T]) DeleteWhere(ctx context.Context, filter bson.M, op string) error {
	if _, err := s.coll.DeleteMany(ctx, filter); err != nil {
		return fmt.Errorf("%s: %w", s.msg(op), err)
	}
	return nil
}
