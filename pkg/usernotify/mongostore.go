package usernotify

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// PrefsCollectionName backs the cloud-mode PrefsStore.
const PrefsCollectionName = "notification_prefs"

// SentCollectionName backs the cloud-mode SentStore.
const SentCollectionName = "sent_notifications"

// sentTTL bounds how long an episode claim is retained. It only needs to
// outlive the reconciliation sweep's lookback window; 30 days matches the
// audit-log retention order of magnitude.
const sentTTL = 30 * 24 * time.Hour

// MongoPrefsStore is the cloud-mode PrefsStore.
type MongoPrefsStore struct {
	coll *mongo.Collection
}

func NewMongoPrefsStore(db *mongo.Database) *MongoPrefsStore {
	return &MongoPrefsStore{coll: db.Collection(PrefsCollectionName)}
}

func (s *MongoPrefsStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "user_id", Value: 1}}, Options: options.Index().SetName("tenant_user").SetUnique(true)},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "scope", Value: 1}}, Options: options.Index().SetName("tenant_scope")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("usernotify: ensure prefs indexes: %w", err)
	}
	return nil
}

func (s *MongoPrefsStore) Get(ctx context.Context, tenantID, userID string) (*Prefs, error) {
	var p Prefs
	err := s.coll.FindOne(ctx, bson.M{"tenant_id": tenantID, "user_id": userID}).Decode(&p)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, fmt.Errorf("usernotify: get prefs: %w", err)
	}
	return &p, nil
}

func (s *MongoPrefsStore) Set(ctx context.Context, p *Prefs) error {
	_, err := s.coll.UpdateOne(ctx,
		bson.M{"tenant_id": p.TenantID, "user_id": p.UserID},
		bson.M{"$set": bson.M{"scope": p.Scope}},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("usernotify: set prefs: %w", err)
	}
	return nil
}

func (s *MongoPrefsStore) ListTeamWide(ctx context.Context, tenantID string) ([]string, error) {
	rows, err := mongoutil.FindAllSorted[Prefs](ctx, s.coll,
		bson.M{"tenant_id": tenantID, "scope": ScopeTeam}, "user_id",
		"usernotify: list team-wide prefs", "usernotify: decode team-wide prefs")
	if err != nil {
		return nil, err
	}
	users := make([]string, 0, len(rows))
	for _, p := range rows {
		users = append(users, p.UserID)
	}
	return users, nil
}

// MongoSentStore is the cloud-mode SentStore. The unique _id insert is the
// first-writer-wins claim shared by every server replica.
type MongoSentStore struct {
	coll *mongo.Collection
}

func NewMongoSentStore(db *mongo.Database) *MongoSentStore {
	return &MongoSentStore{coll: db.Collection(SentCollectionName)}
}

func (s *MongoSentStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "sent_at", Value: 1}},
		Options: options.Index().SetName("sent_at_ttl").SetExpireAfterSeconds(int32(sentTTL / time.Second)),
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("usernotify: ensure sent index: %w", err)
	}
	return nil
}

func (s *MongoSentStore) TryMark(ctx context.Context, key string) (bool, error) {
	_, err := s.coll.InsertOne(ctx, SentRecord{Key: key, SentAt: time.Now().UTC()})
	if err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return false, nil
		}
		return false, fmt.Errorf("usernotify: claim episode: %w", err)
	}
	return true, nil
}

func (s *MongoSentStore) IsMarked(ctx context.Context, key string) (bool, error) {
	n, err := s.coll.CountDocuments(ctx, bson.M{"_id": key})
	if err != nil {
		return false, fmt.Errorf("usernotify: check episode claim: %w", err)
	}
	return n > 0, nil
}

func (s *MongoSentStore) Unmark(ctx context.Context, key string) error {
	if _, err := s.coll.DeleteOne(ctx, bson.M{"_id": key}); err != nil {
		return fmt.Errorf("usernotify: release episode: %w", err)
	}
	return nil
}
