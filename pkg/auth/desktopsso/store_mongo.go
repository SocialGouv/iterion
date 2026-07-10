package desktopsso

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

// TicketsCollectionName is the Mongo collection backing desktop SSO tickets.
const TicketsCollectionName = "desktop_sso_tickets"

// MongoStore is a Mongo-backed Store. The in-memory store is per-process, so a
// ticket minted by the OIDC callback on replica A and redeemed by the desktop
// exchange on replica B would miss. This store shares tickets across replicas;
// expired rows are reaped by a Mongo TTL index AND re-checked on Redeem (TTL
// deletion is lazy, ~60s). FindOneAndDelete gives atomic single-use.
type MongoStore struct {
	coll *mongo.Collection
	ttl  time.Duration
}

// mongoTicketDoc keys the ticket as _id and carries an absolute expiry for the
// TTL index. The LoginResult (incl. its freshly-minted tokens) lives at rest
// only for the short TTL window — the same posture as oidc_states, which holds
// the PKCE verifier.
type mongoTicketDoc struct {
	Ticket    string           `bson:"_id"`
	Result    auth.LoginResult `bson:"result"`
	ExpiresAt time.Time        `bson:"expires_at"`
}

// NewMongoStore wires a Mongo-backed ticket store with a per-entry TTL.
func NewMongoStore(db *mongo.Database, ttl time.Duration) *MongoStore {
	if ttl <= 0 {
		ttl = 2 * time.Minute
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
		return fmt.Errorf("desktopsso: ensure desktop_sso_tickets indexes: %w", err)
	}
	return nil
}

func (s *MongoStore) Mint(ctx context.Context, res auth.LoginResult) (string, error) {
	ticket, err := newTicket()
	if err != nil {
		return "", err
	}
	doc := mongoTicketDoc{Ticket: ticket, Result: res, ExpiresAt: time.Now().Add(s.ttl)}
	if _, err := s.coll.InsertOne(ctx, doc); err != nil {
		return "", fmt.Errorf("desktopsso: mint ticket: %w", err)
	}
	return ticket, nil
}

func (s *MongoStore) Redeem(ctx context.Context, ticket string) (auth.LoginResult, error) {
	var doc mongoTicketDoc
	err := s.coll.FindOneAndDelete(ctx, bson.M{"_id": ticket}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return auth.LoginResult{}, ErrTicketNotFound
	}
	if err != nil {
		return auth.LoginResult{}, fmt.Errorf("desktopsso: redeem ticket: %w", err)
	}
	if time.Now().After(doc.ExpiresAt) {
		return auth.LoginResult{}, ErrTicketNotFound
	}
	return doc.Result, nil
}
