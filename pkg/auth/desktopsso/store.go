// Package desktopsso holds the single-use, TTL-bounded ticket store that
// carries a login result between the OIDC callback (which MINTS a ticket for a
// desktop SSO flow) and the desktop exchange endpoint (which REDEEMS it). The
// two can land on different replicas behind a load balancer, so the store has
// a Mongo backend for the cloud control plane in addition to the in-memory
// default used by the single-binary / local-desktop case — exactly mirroring
// pkg/auth/oidc's StateStore.
package desktopsso

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

// ErrTicketNotFound is returned by Redeem for an unknown, already-redeemed, or
// expired ticket. Callers treat all three identically (401).
var ErrTicketNotFound = errors.New("desktopsso: ticket not found")

// Store mints and single-use-redeems desktop SSO tickets.
type Store interface {
	// Mint stores res under a fresh random ticket and returns the ticket.
	Mint(ctx context.Context, res auth.LoginResult) (string, error)
	// Redeem returns the LoginResult for ticket and invalidates it (single
	// use). Returns ErrTicketNotFound when unknown / expired / already used.
	Redeem(ctx context.Context, ticket string) (auth.LoginResult, error)
}

// MemoryStore is the per-process default (single instance / local desktop).
type MemoryStore struct {
	kit *storekit.TicketMemory[auth.LoginResult]
	ttl time.Duration
}

// NewMemoryStore returns an in-memory ticket store with the given TTL.
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &MemoryStore{kit: storekit.NewTicketMemory[auth.LoginResult](), ttl: ttl}
}

func (s *MemoryStore) Mint(_ context.Context, res auth.LoginResult) (string, error) {
	return s.kit.Mint(res, s.ttl)
}

func (s *MemoryStore) Redeem(_ context.Context, ticket string) (auth.LoginResult, error) {
	res, ok := s.kit.Redeem(ticket)
	if !ok {
		return auth.LoginResult{}, ErrTicketNotFound
	}
	return res, nil
}
