package wsticket

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

// TicketsCollectionName is the Mongo collection backing WS tickets.
const TicketsCollectionName = "ws_tickets"

// MongoStore is a Mongo-backed Store: a ticket minted by POST /api/ws/ticket on
// replica A and redeemed by the WS upgrade on replica B would miss the
// per-process memory store. FindOneAndDelete gives atomic single-use; a TTL
// index reaps stale rows (re-checked on Redeem, TTL deletion is lazy).
type MongoStore struct {
	kit *storekit.TicketMongo[auth.Identity]
	ttl time.Duration
}

// NewMongoStore wires a Mongo-backed WS-ticket store with a per-entry TTL.
func NewMongoStore(db *mongo.Database, ttl time.Duration) *MongoStore {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &MongoStore{
		kit: storekit.NewTicketMongo[auth.Identity](db.Collection(TicketsCollectionName), "identity"),
		ttl: ttl,
	}
}

// EnsureSchema creates the TTL index on expires_at.
func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	return s.kit.EnsureSchema(ctx, "wsticket: ensure ws_tickets indexes")
}

func (s *MongoStore) Mint(ctx context.Context, id auth.Identity) (string, error) {
	return s.kit.Mint(ctx, id, s.ttl, "wsticket: mint")
}

func (s *MongoStore) Redeem(ctx context.Context, ticket string) (auth.Identity, error) {
	return s.kit.Redeem(ctx, ticket, ErrTicketNotFound, "wsticket: redeem")
}
