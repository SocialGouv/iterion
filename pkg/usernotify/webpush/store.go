// Package webpush is the Web Push (RFC 8030 + VAPID) usernotify.Sink: it
// delivers a Notification to every browser PushSubscription registered by
// each recipient, via the browsers' push services.
package webpush

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// Subscription is one browser push registration (one user × one browser
// profile × one device). The endpoint is the push service's capability URL
// and is globally unique. A subscription belongs to the USER, not a team:
// TenantID records where it was enrolled (provenance) but delivery must
// not filter on it — a multi-team user enrolls once and receives
// notifications for runs of every team that resolves them as a recipient
// (recipient resolution is already tenant-scoped upstream).
type Subscription struct {
	ID         string    `json:"id" bson:"_id"`
	TenantID   string    `json:"tenant_id" bson:"tenant_id"`
	UserID     string    `json:"user_id" bson:"user_id"`
	Endpoint   string    `json:"endpoint" bson:"endpoint"`
	P256dh     string    `json:"p256dh" bson:"p256dh"`
	Auth       string    `json:"auth" bson:"auth"`
	UserAgent  string    `json:"user_agent,omitempty" bson:"user_agent,omitempty"`
	CreatedAt  time.Time `json:"created_at" bson:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty" bson:"last_used_at,omitempty"`
}

// SubscriptionStore persists browser push registrations.
type SubscriptionStore interface {
	// Upsert registers (or refreshes) a subscription, keyed on its endpoint.
	Upsert(ctx context.Context, s *Subscription) error
	// ListForUsers returns every subscription of the given users in one
	// query (a notification fans out to all recipients at once). Not
	// tenant-filtered — see the Subscription doc.
	ListForUsers(ctx context.Context, userIDs []string) ([]*Subscription, error)
	// DeleteByEndpoint removes the caller's own subscription (unsubscribe).
	DeleteByEndpoint(ctx context.Context, userID, endpoint string) error
	// Prune removes a subscription the push service reported dead (404/410).
	Prune(ctx context.Context, endpoint string) error
	// Touch refreshes last_used_at after a successful delivery. Best-effort.
	Touch(ctx context.Context, endpoint string, at time.Time) error
}

// MemSubscriptionStore is the in-memory SubscriptionStore for tests and
// local mode.
type MemSubscriptionStore struct {
	mu   sync.RWMutex
	rows map[string]Subscription // by endpoint
}

func NewMemSubscriptionStore() *MemSubscriptionStore {
	return &MemSubscriptionStore{rows: make(map[string]Subscription)}
}

func (m *MemSubscriptionStore) Upsert(_ context.Context, s *Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *s
	if prev, ok := m.rows[s.Endpoint]; ok {
		cp.ID = prev.ID
		cp.CreatedAt = prev.CreatedAt
	} else {
		if cp.ID == "" {
			cp.ID = s.Endpoint
		}
		if cp.CreatedAt.IsZero() {
			cp.CreatedAt = time.Now().UTC()
		}
	}
	m.rows[s.Endpoint] = cp
	return nil
}

func (m *MemSubscriptionStore) ListForUsers(_ context.Context, userIDs []string) ([]*Subscription, error) {
	users := make(map[string]struct{}, len(userIDs))
	for _, u := range userIDs {
		users[u] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Subscription
	for _, s := range m.rows {
		if _, ok := users[s.UserID]; ok {
			cp := s
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *MemSubscriptionStore) DeleteByEndpoint(_ context.Context, userID, endpoint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.rows[endpoint]; ok && s.UserID == userID {
		delete(m.rows, endpoint)
	}
	return nil
}

func (m *MemSubscriptionStore) Prune(_ context.Context, endpoint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, endpoint)
	return nil
}

func (m *MemSubscriptionStore) Touch(_ context.Context, endpoint string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.rows[endpoint]; ok {
		s.LastUsedAt = at
		m.rows[endpoint] = s
	}
	return nil
}

// SubscriptionsCollectionName backs the cloud-mode SubscriptionStore.
const SubscriptionsCollectionName = "push_subscriptions"

// MongoSubscriptionStore is the cloud-mode SubscriptionStore.
type MongoSubscriptionStore struct {
	coll *mongo.Collection
}

func NewMongoSubscriptionStore(db *mongo.Database) *MongoSubscriptionStore {
	return &MongoSubscriptionStore{coll: db.Collection(SubscriptionsCollectionName)}
}

func (s *MongoSubscriptionStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "endpoint", Value: 1}}, Options: options.Index().SetName("endpoint").SetUnique(true)},
		{Keys: bson.D{{Key: "user_id", Value: 1}}, Options: options.Index().SetName("user")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("webpush: ensure subscription indexes: %w", err)
	}
	return nil
}

func (s *MongoSubscriptionStore) Upsert(ctx context.Context, sub *Subscription) error {
	now := time.Now().UTC()
	_, err := s.coll.UpdateOne(ctx,
		bson.M{"endpoint": sub.Endpoint},
		bson.M{
			"$set": bson.M{
				"tenant_id":  sub.TenantID,
				"user_id":    sub.UserID,
				"p256dh":     sub.P256dh,
				"auth":       sub.Auth,
				"user_agent": sub.UserAgent,
			},
			"$setOnInsert": bson.M{"_id": sub.Endpoint, "created_at": now},
		},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("webpush: upsert subscription: %w", err)
	}
	return nil
}

func (s *MongoSubscriptionStore) ListForUsers(ctx context.Context, userIDs []string) ([]*Subscription, error) {
	return mongoutil.FindAllSorted[*Subscription](ctx, s.coll,
		bson.M{"user_id": bson.M{"$in": userIDs}}, "created_at",
		"webpush: list subscriptions", "webpush: decode subscriptions")
}

func (s *MongoSubscriptionStore) DeleteByEndpoint(ctx context.Context, userID, endpoint string) error {
	if _, err := s.coll.DeleteOne(ctx, bson.M{"endpoint": endpoint, "user_id": userID}); err != nil {
		return fmt.Errorf("webpush: delete subscription: %w", err)
	}
	return nil
}

func (s *MongoSubscriptionStore) Prune(ctx context.Context, endpoint string) error {
	if _, err := s.coll.DeleteOne(ctx, bson.M{"endpoint": endpoint}); err != nil {
		return fmt.Errorf("webpush: prune subscription: %w", err)
	}
	return nil
}

func (s *MongoSubscriptionStore) Touch(ctx context.Context, endpoint string, at time.Time) error {
	if _, err := s.coll.UpdateOne(ctx, bson.M{"endpoint": endpoint}, bson.M{"$set": bson.M{"last_used_at": at}}); err != nil {
		return fmt.Errorf("webpush: touch subscription: %w", err)
	}
	return nil
}
