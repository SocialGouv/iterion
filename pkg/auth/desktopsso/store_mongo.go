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
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

// TicketsCollectionName is the Mongo collection backing desktop SSO tickets.
const TicketsCollectionName = "desktop_sso_tickets"

// MongoStore is a Mongo-backed Store. The in-memory store is per-process, so
// a ticket minted by the OIDC callback on replica A and redeemed by the
// desktop exchange on replica B would miss. This store shares tickets across
// replicas; expired rows are reaped by a Mongo TTL index AND re-checked on
// Redeem (TTL deletion is lazy, ~60s). FindOneAndDelete gives atomic
// single-use.
//
// The document is the SHA-256 of the ticket, the deferred login it carries
// and the expiry — NO token: the session is minted at redemption
// (auth.Service.RedeemDesktopLogin), so no refresh token ever rests in this
// collection, in the oplog, or in a backup (#2635). A document written
// before this shape (the ticket in plaintext as _id, a whole LoginResult as
// "result") answers no lookup — its _id is no hash a redemption computes —
// and is reaped by the TTL index.
type MongoStore struct {
	coll *mongo.Collection
	ttl  time.Duration
}

// ticketDoc is the wire shape of a desktop_sso_tickets document.
type ticketDoc struct {
	ID        string            `bson:"_id"`
	Pending   auth.DesktopLogin `bson:"pending"`
	ExpiresAt time.Time         `bson:"expires_at"`
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

// Mint stores d under a fresh random ticket expiring after the store's TTL
// and returns the ticket. The ticket itself is never stored — only its hash
// is (ticketKey).
func (s *MongoStore) Mint(ctx context.Context, d auth.DesktopLogin) (string, error) {
	ticket, err := storekit.NewTicket()
	if err != nil {
		return "", err
	}
	if _, err := s.coll.InsertOne(ctx, ticketDoc{
		ID:        ticketKey(ticket),
		Pending:   d,
		ExpiresAt: time.Now().Add(s.ttl),
	}); err != nil {
		return "", fmt.Errorf("desktopsso: mint ticket: %w", err)
	}
	return ticket, nil
}

// Redeem atomically removes and returns the DesktopLogin stored under
// ticket, mapping unknown / expired tickets to ErrTicketNotFound. An expired
// ticket is still consumed.
func (s *MongoStore) Redeem(ctx context.Context, ticket string) (auth.DesktopLogin, error) {
	var doc ticketDoc
	err := s.coll.FindOneAndDelete(ctx, bson.M{"_id": ticketKey(ticket)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return auth.DesktopLogin{}, ErrTicketNotFound
	}
	if err != nil {
		return auth.DesktopLogin{}, fmt.Errorf("desktopsso: redeem ticket: %w", err)
	}
	// A missing or non-positive expires_at reads as expired.
	if doc.ExpiresAt.IsZero() || time.Now().After(doc.ExpiresAt) {
		return auth.DesktopLogin{}, ErrTicketNotFound
	}
	return doc.Pending, nil
}
