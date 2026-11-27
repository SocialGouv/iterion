package pat

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

const colTokens = "personal_access_tokens"

// MongoStore is the production PAT store.
type MongoStore struct{ kit *storekit.Mongo[Token] }

func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{kit: storekit.NewMongo[Token](db.Collection(colTokens), ErrNotFound, "pat")}
}

// EnsureSchema creates the PAT indexes idempotently.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	col := db.Collection(colTokens)
	if _, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("token_hash_unique")},
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("user_recent")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("pat: ensure indexes: %w", err)
	}
	return nil
}

func (s *MongoStore) Create(ctx context.Context, t Token) error {
	return s.kit.Insert(ctx, t, nil, "insert")
}

func (s *MongoStore) GetByTokenHash(ctx context.Context, hash string) (Token, error) {
	return s.kit.FindOne(ctx, bson.M{"token_hash": hash}, "get by hash")
}

func (s *MongoStore) Get(ctx context.Context, id string) (Token, error) {
	return s.kit.GetByID(ctx, id, "get")
}

func (s *MongoStore) ListByUser(ctx context.Context, userID string) ([]Token, error) {
	return s.kit.List(ctx, bson.M{"user_id": userID}, "list", "decode",
		options.Find().SetSort(bson.M{"created_at": -1}))
}

func (s *MongoStore) Revoke(ctx context.Context, id string, at time.Time) error {
	res, err := s.kit.Coll().UpdateOne(ctx,
		bson.M{"_id": id, "revoked_at": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"revoked_at": at}})
	if err != nil {
		return fmt.Errorf("pat: revoke %s: %w", id, err)
	}
	if res.MatchedCount == 1 {
		return nil
	}
	// Nothing matched: a revoked token keeps its first instant and the
	// revoke stands; a token that does not exist answers ErrNotFound.
	_, err = s.kit.GetByID(ctx, id, "revoke")
	return err
}

func (s *MongoStore) RevokeIfLive(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.kit.Coll().UpdateOne(ctx,
		bson.M{"_id": id, "revoked_at": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"revoked_at": at}})
	if err != nil {
		return false, fmt.Errorf("pat: revoke %s: %w", id, err)
	}
	if res.ModifiedCount == 1 {
		return true, nil
	}
	// Nothing modified: a revoked token was retired by another call (or
	// before), a token that does not exist answers ErrNotFound.
	if _, err := s.kit.GetByID(ctx, id, "revoke"); err != nil {
		return false, err
	}
	return false, nil
}

func (s *MongoStore) RevokeAllByUser(ctx context.Context, userID string, at time.Time) (int64, error) {
	res, err := s.kit.Coll().UpdateMany(ctx,
		bson.M{"user_id": userID, "revoked_at": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"revoked_at": at}})
	if err != nil {
		return 0, fmt.Errorf("pat: revoke the tokens of user %s: %w", userID, err)
	}
	return res.ModifiedCount, nil
}

func (s *MongoStore) Disable(ctx context.Context, id string, at time.Time) error {
	res, err := s.kit.Coll().UpdateOne(ctx,
		bson.M{"_id": id, "revoked_at": bson.M{"$exists": false}, "disabled_at": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"disabled_at": at}})
	if err != nil {
		return fmt.Errorf("pat: disable %s: %w", id, err)
	}
	if res.MatchedCount == 1 {
		return nil
	}
	// Nothing matched: a token already disabled keeps its first instant, a
	// revoked one is refused, one that does not exist answers ErrNotFound.
	t, err := s.kit.GetByID(ctx, id, "disable")
	if err != nil {
		return err
	}
	if t.RevokedAt != nil {
		return ErrRevoked
	}
	return nil
}

func (s *MongoStore) Enable(ctx context.Context, id string) error {
	res, err := s.kit.Coll().UpdateOne(ctx,
		bson.M{"_id": id, "revoked_at": bson.M{"$exists": false}},
		bson.M{"$unset": bson.M{"disabled_at": ""}})
	if err != nil {
		return fmt.Errorf("pat: enable %s: %w", id, err)
	}
	if res.MatchedCount == 1 {
		return nil
	}
	t, err := s.kit.GetByID(ctx, id, "enable")
	if err != nil {
		return err
	}
	if t.RevokedAt != nil {
		return ErrRevoked
	}
	// Matched nothing yet neither gone nor revoked: the row changed between
	// the two reads, which this call does not decide for the caller.
	return fmt.Errorf("pat: enable %s: the row changed during the call, re-issue it", id)
}

func (s *MongoStore) Purge(ctx context.Context, id string) error {
	res, err := s.kit.Coll().DeleteOne(ctx, bson.M{"_id": id, "revoked_at": bson.M{"$exists": true}})
	if err != nil {
		return fmt.Errorf("pat: purge %s: %w", id, err)
	}
	if res.DeletedCount == 1 {
		return nil
	}
	if _, err := s.kit.GetByID(ctx, id, "purge"); err != nil {
		return err
	}
	return ErrNotRevoked
}

// MarkUsed is a best-effort stamp: a token revoked-and-purged mid-request
// is not an error here.
func (s *MongoStore) MarkUsed(ctx context.Context, id string, at time.Time) error {
	return s.kit.SetAny(ctx, id, bson.M{"last_used_at": at}, "mark used")
}
