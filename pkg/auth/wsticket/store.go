// Package wsticket holds the single-use, short-TTL ticket store that lets a
// client open an authenticated WebSocket without carrying a long-lived access
// JWT in the URL (query strings leak to access logs, proxies, and Referer).
// The client mints a ticket with an authenticated POST /api/ws/ticket, passes
// it as ?ticket= on the WS dial, and the server consumes + invalidates it
// before the upgrade. Mirrors pkg/auth/desktopsso (a separate store because it
// carries an Identity, not a LoginResult) with an in-memory default and a
// Mongo backend so mint + redeem can land on different replicas.
package wsticket

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

// ErrTicketNotFound is returned by Redeem for an unknown, already-redeemed, or
// expired ticket. Callers treat all three identically (401 / no upgrade).
var ErrTicketNotFound = errors.New("wsticket: ticket not found")

// Store mints and single-use-redeems WS tickets bound to an identity.
type Store interface {
	Mint(ctx context.Context, id auth.Identity) (string, error)
	Redeem(ctx context.Context, ticket string) (auth.Identity, error)
}

// MemoryStore is the per-process default (single instance / local desktop).
type MemoryStore struct {
	kit *storekit.TicketMemory[auth.Identity]
	ttl time.Duration
}

// NewMemoryStore returns an in-memory WS-ticket store with the given TTL.
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &MemoryStore{kit: storekit.NewTicketMemory[auth.Identity](), ttl: ttl}
}

func (s *MemoryStore) Mint(_ context.Context, id auth.Identity) (string, error) {
	return s.kit.Mint(id, s.ttl)
}

func (s *MemoryStore) Redeem(_ context.Context, ticket string) (auth.Identity, error) {
	id, ok := s.kit.Redeem(ticket)
	if !ok {
		return auth.Identity{}, ErrTicketNotFound
	}
	return id, nil
}
