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
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
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

// newTicket returns a 32-byte base64url random ticket.
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
	result auth.LoginResult
	expiry time.Time
}

// NewMemoryStore returns an in-memory ticket store with the given TTL.
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &MemoryStore{m: make(map[string]memEntry), ttl: ttl}
}

func (s *MemoryStore) Mint(_ context.Context, res auth.LoginResult) (string, error) {
	ticket, err := newTicket()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.m[ticket] = memEntry{result: res, expiry: time.Now().Add(s.ttl)}
	return ticket, nil
}

func (s *MemoryStore) Redeem(_ context.Context, ticket string) (auth.LoginResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[ticket]
	if !ok {
		return auth.LoginResult{}, ErrTicketNotFound
	}
	delete(s.m, ticket)
	if time.Now().After(e.expiry) {
		return auth.LoginResult{}, ErrTicketNotFound
	}
	return e.result, nil
}

func (s *MemoryStore) sweepLocked() {
	now := time.Now()
	for k, v := range s.m {
		if now.After(v.expiry) {
			delete(s.m, k)
		}
	}
}
