package storekit

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// NewTicket returns a 32-byte base64url random token — the shared mint
// primitive behind the single-use ticket stores (desktopsso, wsticket).
func NewTicket() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// TicketMemory is the per-process single-use TTL ticket store: Mint
// stores a payload under a fresh random ticket, Redeem consumes it
// exactly once. Expired entries are swept opportunistically on Mint and
// re-checked on Redeem.
type TicketMemory[T any] struct {
	mu sync.Mutex
	m  map[string]ticketEntry[T]
}

type ticketEntry[T any] struct {
	payload T
	expiry  time.Time
}

// NewTicketMemory returns an empty in-memory ticket store. The TTL is
// per-Mint so the domain wrapper keeps owning its default.
func NewTicketMemory[T any]() *TicketMemory[T] {
	return &TicketMemory[T]{m: make(map[string]ticketEntry[T])}
}

// Mint sweeps expired entries, stores v under a fresh random ticket
// expiring after ttl, and returns the ticket.
func (s *TicketMemory[T]) Mint(v T, ttl time.Duration) (string, error) {
	ticket, err := NewTicket()
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
	s.m[ticket] = ticketEntry[T]{payload: v, expiry: now.Add(ttl)}
	return ticket, nil
}

// Redeem removes and returns the payload stored under ticket (single
// use). ok is false for an unknown or expired ticket; an expired ticket
// is still consumed.
func (s *TicketMemory[T]) Redeem(ticket string) (T, bool) {
	var zero T
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[ticket]
	if !ok {
		return zero, false
	}
	delete(s.m, ticket)
	if time.Now().After(e.expiry) {
		return zero, false
	}
	return e.payload, true
}
