package wsticket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// TicketsCollectionName is the Mongo collection backing WS tickets.
const TicketsCollectionName = "ws_tickets"

// MongoStore is a Mongo-backed Store: a ticket minted by POST /api/ws/ticket on
// replica A and redeemed by the WS upgrade on replica B would miss the
// per-process memory store. FindOneAndDelete gives atomic single-use; a TTL
// index reaps stale rows (re-checked on Redeem, TTL deletion is lazy).
type MongoStore struct {
	coll *mongo.Collection
	ttl  time.Duration
}

type mongoTicketDoc struct {
	Ticket    string        `bson:"_id"`
	Identity  auth.Identity `bson:"identity"`
	ExpiresAt time.Time     `bson:"expires_at"`
}

// NewMongoStore wires a Mongo-backed WS-ticket store with a per-entry TTL.
func NewMongoStore(db *mongo.Database, ttl time.Duration) *MongoStore {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &MongoStore{coll: db.Collection(TicketsCollectionName), ttl: ttl}
}

// EnsureSchema creates the TTL index on expires_at.
func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "expires_at", Value: 1}},
			Options: options.Index().SetName("ttl_expires_at").SetExpireAfterSeconds(0),
		},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("wsticket: ensure ws_tickets indexes: %w", err)
	}
	return nil
}

func (s *MongoStore) Mint(ctx context.Context, id auth.Identity) (string, error) {
	ticket, err := newTicket()
	if err != nil {
		return "", err
	}
	doc := mongoTicketDoc{Ticket: ticket, Identity: id, ExpiresAt: time.Now().Add(s.ttl)}
	if _, err := s.coll.InsertOne(ctx, doc); err != nil {
		return "", fmt.Errorf("wsticket: mint: %w", err)
	}
	return ticket, nil
}

func (s *MongoStore) Redeem(ctx context.Context, ticket string) (auth.Identity, error) {
	var doc mongoTicketDoc
	err := s.coll.FindOneAndDelete(ctx, bson.M{"_id": ticket}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return auth.Identity{}, ErrTicketNotFound
	}
	if err != nil {
		return auth.Identity{}, fmt.Errorf("wsticket: redeem: %w", err)
	}
	if time.Now().After(doc.ExpiresAt) {
		return auth.Identity{}, ErrTicketNotFound
	}
	return doc.Identity, nil
}
