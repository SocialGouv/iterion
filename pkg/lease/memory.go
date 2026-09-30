package lease

import (
	"context"
	"strings"
	"sync"
	"time"
)

// MemoryStore is the in-process Store: tests, and a single-process server where
// every lease is trivially this process's own. Same semantics as MongoStore;
// the zero value is ready to use.
type MemoryStore struct {
	mu     sync.Mutex
	leases map[string]memoryLease
}

type memoryLease struct {
	owner     string
	expiresAt time.Time
}

// NewMemoryStore returns an empty in-memory lease store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{leases: map[string]memoryLease{}}
}

var _ Store = (*MemoryStore)(nil)

// A call on a done context fails and changes nothing, as a store round-trip
// does: every stop path of Run depends on which calls still reach the store.

func (s *MemoryStore) Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	if err := validate(name, owner, ttl); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	name, owner, now = strings.TrimSpace(name), strings.TrimSpace(owner), stamp(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	l, held := s.leases[name]
	// Held means "by someone else, and not yet expired at now" — the same
	// predicate the Mongo filter negates (expires_at <= now frees it).
	if held && l.owner != owner && l.expiresAt.After(now) {
		return false, nil
	}
	expires := stamp(now.Add(ttl))
	if held && l.owner == owner && l.expiresAt.After(expires) {
		expires = l.expiresAt // an older stamp arriving late never shortens the lease
	}
	if s.leases == nil {
		s.leases = map[string]memoryLease{}
	}
	s.leases[name] = memoryLease{owner: owner, expiresAt: expires}
	return true, nil
}

func (s *MemoryStore) Renew(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	if err := validate(name, owner, ttl); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	name, owner, now = strings.TrimSpace(name), strings.TrimSpace(owner), stamp(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.leases[name]
	if !ok || l.owner != owner {
		return false, nil
	}
	if expires := stamp(now.Add(ttl)); expires.After(l.expiresAt) {
		l.expiresAt = expires
	}
	s.leases[name] = l
	return true, nil
}

func (s *MemoryStore) Release(ctx context.Context, name, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, owner = strings.TrimSpace(name), strings.TrimSpace(owner)
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.leases[name]
	if !ok {
		return nil
	}
	if l.owner != owner {
		return ErrLost
	}
	delete(s.leases, name)
	return nil
}
