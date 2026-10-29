// Package desktopsso holds the single-use, TTL-bounded ticket store that
// carries a DEFERRED login between the OIDC callback (which MINTS a ticket
// for a desktop SSO flow) and the desktop exchange endpoint (which REDEEMS
// it). The two can land on different replicas behind a load balancer, so the
// store has a Mongo backend for the cloud control plane in addition to the
// in-memory default used by the single-binary / local-desktop case — exactly
// mirroring pkg/auth/oidc's StateStore.
//
// The payload is an auth.DesktopLogin: what the redemption needs to mint the
// session AT REDEMPTION — the account, the lineage the session will be born
// under, the landing team. No token is ever stored: the refresh token does
// not exist until the redemption mints it (#2635), so a reader of the store
// (a backup, a misconfigured replica, an operator query) finds no credential
// at rest, and an unredeemed ticket leaves no live session behind — there is
// none to revoke at its expiry. The ticket itself is a one-time secret and
// is stored only as its SHA-256 (ticketKey): the store can answer "is this
// ticket yours, once" without holding anything that redeems.
package desktopsso

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

// ErrTicketNotFound is returned by Redeem for an unknown, already-redeemed, or
// expired ticket. Callers treat all three identically (401).
var ErrTicketNotFound = errors.New("desktopsso: ticket not found")

// Store mints and single-use-redeems desktop SSO tickets.
type Store interface {
	// Mint stores d under a fresh random ticket and returns the ticket.
	Mint(ctx context.Context, d auth.DesktopLogin) (string, error)
	// Redeem returns the DesktopLogin for ticket and invalidates it (single
	// use). Returns ErrTicketNotFound when unknown / expired / already used.
	Redeem(ctx context.Context, ticket string) (auth.DesktopLogin, error)
}

// ticketKey is the key a ticket is stored under: the SHA-256 of the ticket,
// hexed — never the ticket itself. The ticket is a 256-bit one-time secret;
// its hash is what a dump of the store holds, and a hash redeems nothing
// (the same posture sessions take with the refresh token's hash).
func ticketKey(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// MemoryStore is the per-process default (single instance / local desktop).
type MemoryStore struct {
	mu  sync.Mutex
	m   map[string]memoryTicket
	ttl time.Duration
}

// memoryTicket is one stored ticket: the deferred login and its absolute
// expiry.
type memoryTicket struct {
	pending auth.DesktopLogin
	expiry  time.Time
}

// NewMemoryStore returns an in-memory ticket store with the given TTL.
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &MemoryStore{m: make(map[string]memoryTicket), ttl: ttl}
}

// Mint sweeps expired entries, stores d under a fresh random ticket expiring
// after the store's TTL, and returns the ticket.
func (s *MemoryStore) Mint(_ context.Context, d auth.DesktopLogin) (string, error) {
	ticket, err := storekit.NewTicket()
	if err != nil {
		return "", err
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.m {
		if now.After(e.expiry) {
			delete(s.m, k)
		}
	}
	s.m[ticketKey(ticket)] = memoryTicket{pending: d, expiry: now.Add(s.ttl)}
	return ticket, nil
}

// Redeem removes and returns the DesktopLogin stored under ticket (single
// use). ErrTicketNotFound for an unknown or expired ticket; an expired
// ticket is still consumed.
func (s *MemoryStore) Redeem(_ context.Context, ticket string) (auth.DesktopLogin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ticketKey(ticket)
	e, ok := s.m[key]
	if !ok {
		return auth.DesktopLogin{}, ErrTicketNotFound
	}
	delete(s.m, key)
	if time.Now().After(e.expiry) {
		return auth.DesktopLogin{}, ErrTicketNotFound
	}
	return e.pending, nil
}
