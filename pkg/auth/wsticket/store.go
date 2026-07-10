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
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
)

// ErrTicketNotFound is returned by Redeem for an unknown, already-redeemed, or
// expired ticket. Callers treat all three identically (401 / no upgrade).
var ErrTicketNotFound = errors.New("wsticket: ticket not found")

// Store mints and single-use-redeems WS tickets bound to an identity.
type Store interface {
	Mint(ctx context.Context, id auth.Identity) (string, error)
	Redeem(ctx context.Context, ticket string) (auth.Identity, error)
}

func newTicket() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// MemoryStore is the per-process default (single instance / local desktop).
type MemoryStore struct {
	mu  sync.Mutex
	m   map[string]memEntry
	ttl time.Duration
}

type memEntry struct {
	id     auth.Identity
	expiry time.Time
}

// NewMemoryStore returns an in-memory WS-ticket store with the given TTL.
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &MemoryStore{m: make(map[string]memEntry), ttl: ttl}
}

func (s *MemoryStore) Mint(_ context.Context, id auth.Identity) (string, error) {
	ticket, err := newTicket()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.m[ticket] = memEntry{id: id, expiry: time.Now().Add(s.ttl)}
	return ticket, nil
}

func (s *MemoryStore) Redeem(_ context.Context, ticket string) (auth.Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[ticket]
	if !ok {
		return auth.Identity{}, ErrTicketNotFound
	}
	delete(s.m, ticket)
	if time.Now().After(e.expiry) {
		return auth.Identity{}, ErrTicketNotFound
	}
	return e.id, nil
}

func (s *MemoryStore) sweepLocked() {
	now := time.Now()
	for k, v := range s.m {
		if now.After(v.expiry) {
			delete(s.m, k)
		}
	}
}
