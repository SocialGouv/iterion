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
	return s.kit.Set(ctx, id, bson.M{"revoked_at": at}, "revoke")
}

// MarkUsed is a best-effort stamp: a token revoked-and-purged mid-request
// is not an error here.
func (s *MongoStore) MarkUsed(ctx context.Context, id string, at time.Time) error {
	return s.kit.SetAny(ctx, id, bson.M{"last_used_at": at}, "mark used")
}
