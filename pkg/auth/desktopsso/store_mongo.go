package desktopsso

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

// TicketsCollectionName is the Mongo collection backing desktop SSO tickets.
const TicketsCollectionName = "desktop_sso_tickets"

// MongoStore is a Mongo-backed Store. The in-memory store is per-process, so a
// ticket minted by the OIDC callback on replica A and redeemed by the desktop
// exchange on replica B would miss. This store shares tickets across replicas;
// expired rows are reaped by a Mongo TTL index AND re-checked on Redeem (TTL
// deletion is lazy, ~60s). FindOneAndDelete gives atomic single-use. The
// LoginResult (incl. its freshly-minted tokens) lives at rest only for the
// short TTL window — the same posture as oidc_states, which holds the PKCE
// verifier.
type MongoStore struct {
	kit *storekit.TicketMongo[auth.LoginResult]
	ttl time.Duration
}

// NewMongoStore wires a Mongo-backed ticket store with a per-entry TTL.
func NewMongoStore(db *mongo.Database, ttl time.Duration) *MongoStore {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &MongoStore{
		kit: storekit.NewTicketMongo[auth.LoginResult](db.Collection(TicketsCollectionName), "result"),
		ttl: ttl,
	}
}

// EnsureSchema creates the TTL index on expires_at.
func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	return s.kit.EnsureSchema(ctx, "desktopsso: ensure desktop_sso_tickets indexes")
}

func (s *MongoStore) Mint(ctx context.Context, res auth.LoginResult) (string, error) {
	return s.kit.Mint(ctx, res, s.ttl, "desktopsso: mint ticket")
}

func (s *MongoStore) Redeem(ctx context.Context, ticket string) (auth.LoginResult, error) {
	return s.kit.Redeem(ctx, ticket, ErrTicketNotFound, "desktopsso: redeem ticket")
}
