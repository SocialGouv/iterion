package connection

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store persists connections.
//
// Every read takes the tenant as its FIRST argument, and that is the whole
// design. `forge.ConnectionStore.Get(ctx, id)` filters on `_id` alone, so any
// caller holding an id — a run, a webhook, an API path parameter — reads any
// tenant's connection, and nothing at the call site looks wrong. Making the
// tenant positional means a caller cannot forget it: they must produce one,
// and producing the wrong one is a mistake a reviewer can see.
//
// A connection belonging to another tenant answers ErrNotFound, never a
// distinguishable "forbidden": to an untrusted caller the two must read the
// same, or the error itself enumerates which ids exist.
type Store interface {
	Create(ctx context.Context, c Connection) error
	// Get returns one connection of tenantID.
	Get(ctx context.Context, tenantID, id string) (Connection, error)
	// ByAlias resolves the name a `.bot` writes in `connection:`. Scoped to
	// one connector as well as one tenant, so two packages may each have a
	// connection called "main".
	ByAlias(ctx context.Context, tenantID, connector, alias string) (Connection, error)
	// List returns a tenant's connections, oldest first. Pass an empty
	// connector for all of them.
	List(ctx context.Context, tenantID, connector string) ([]Connection, error)
	Update(ctx context.Context, c Connection) error
	Delete(ctx context.Context, tenantID, id string) error
}

// MemoryStore is the in-process Store: the local CLI and desktop tier, and
// every test's. Its cloud twin is the Mongo implementation, which lands with
// the same conformance suite — a durable seam that ships filesystem-only is a
// cloud hole rather than a known limitation.
type MemoryStore struct {
	mu    sync.RWMutex
	byID  map[string]Connection
	clock func() time.Time
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: map[string]Connection{}, clock: time.Now}
}

// WithClock pins the clock, so a test can assert on timestamps.
func (s *MemoryStore) WithClock(now func() time.Time) *MemoryStore {
	s.clock = now
	return s
}

func (s *MemoryStore) Create(_ context.Context, c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[c.ID]; exists {
		return ErrExists
	}
	// An alias is how a `.bot` names a connection, so a duplicate would make
	// the workflow's choice depend on iteration order.
	for _, other := range s.byID {
		if other.TenantID == c.TenantID && other.Connector == c.Connector && aliasEq(other.Alias, c.Alias) {
			return ErrExists
		}
	}
	now := s.clock()
	c.CreatedAt, c.UpdatedAt = now, now
	s.byID[c.ID] = c
	return nil
}

func (s *MemoryStore) Get(_ context.Context, tenantID, id string) (Connection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.byID[id]
	// The tenant check is the point of the signature. A record that exists but
	// belongs elsewhere is NOT FOUND, not forbidden.
	if !ok || c.TenantID != tenantID {
		return Connection{}, ErrNotFound
	}
	return c, nil
}

func (s *MemoryStore) ByAlias(_ context.Context, tenantID, connector, alias string) (Connection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.byID {
		if c.TenantID == tenantID && c.Connector == connector && aliasEq(c.Alias, alias) {
			return c, nil
		}
	}
	return Connection{}, ErrNotFound
}

func (s *MemoryStore) List(_ context.Context, tenantID, connector string) ([]Connection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Connection
	for _, c := range s.byID {
		if c.TenantID != tenantID {
			continue
		}
		if connector != "" && c.Connector != connector {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *MemoryStore) Update(_ context.Context, c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.byID[c.ID]
	if !ok {
		return ErrNotFound
	}
	// An update may not move a connection between tenants: that would be a
	// silent grant of one tenant's credential to another, written as an
	// ordinary edit.
	if prev.TenantID != c.TenantID {
		return ErrNotFound
	}
	c.CreatedAt = prev.CreatedAt
	c.UpdatedAt = s.clock()
	s.byID[c.ID] = c
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[id]
	if !ok || c.TenantID != tenantID {
		return ErrNotFound
	}
	delete(s.byID, id)
	return nil
}

// aliasEq compares aliases case-insensitively. An operator who created "Main"
// and a `.bot` that writes "main" mean the same connection, and the failure of
// the alternative is a run that cannot find a credential that is plainly there.
func aliasEq(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
