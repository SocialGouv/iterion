package botsource

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/store"
)

// MemoryStore is an in-process Store for tests and local mode.
type MemoryStore struct {
	mu   sync.RWMutex
	byID map[string]BotSource
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: make(map[string]BotSource)}
}

func (m *MemoryStore) Create(_ context.Context, s BotSource) (BotSource, error) {
	if err := s.Validate(); err != nil {
		return BotSource{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.byID {
		if e.TenantID == s.TenantID && e.Slug == s.Slug {
			return BotSource{}, ErrSlugConflict
		}
	}
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	s.CreatedAt, s.UpdatedAt = now, now
	s.Version = 1
	if s.Origin == "" {
		s.Origin = "tenant"
	}
	m.byID[s.ID] = s
	return s, nil
}

func (m *MemoryStore) Get(_ context.Context, id string) (BotSource, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.byID[id]
	if !ok {
		return BotSource{}, ErrNotFound
	}
	return s, nil
}

func (m *MemoryStore) GetBySlug(ctx context.Context, tenantID, slug string) (BotSource, error) {
	if tenantID == "" {
		return BotSource{}, ErrTenantMissing
	}
	// Same sentinel-scoping defense as MongoStore (the stores must not
	// diverge): a mismatched scoped read sees a foreign row as absent.
	if ctxTenant, ok := store.TenantFromContext(ctx); ok && ctxTenant != "" && ctxTenant != tenantID {
		return BotSource{}, fmt.Errorf("botsource: tenant mismatch: ctx=%q arg=%q: %w", ctxTenant, tenantID, ErrNotFound)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.byID {
		if s.TenantID == tenantID && s.Slug == slug {
			return s, nil
		}
	}
	return BotSource{}, ErrNotFound
}

// Update replaces a source's content. If s.Version is non-zero it acts as an
// if-match token: a mismatch against the stored version returns
// ErrVersionConflict. On success the stored version is incremented.
func (m *MemoryStore) Update(_ context.Context, s BotSource) (BotSource, error) {
	if err := s.Validate(); err != nil {
		return BotSource{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, ok := m.byID[s.ID]
	if !ok {
		return BotSource{}, ErrNotFound
	}
	if s.Version != 0 && s.Version != prev.Version {
		return BotSource{}, ErrVersionConflict
	}
	for _, e := range m.byID {
		if e.TenantID == s.TenantID && e.Slug == s.Slug && e.ID != s.ID {
			return BotSource{}, ErrSlugConflict
		}
	}
	s.CreatedAt = prev.CreatedAt
	s.CreatedBy = prev.CreatedBy
	if s.Origin == "" {
		s.Origin = prev.Origin
	}
	s.Version = prev.Version + 1
	s.UpdatedAt = time.Now().UTC()
	m.byID[s.ID] = s
	return s, nil
}

func (m *MemoryStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byID[id]; !ok {
		return ErrNotFound
	}
	delete(m.byID, id)
	return nil
}

func (m *MemoryStore) ListByTenant(ctx context.Context, tenantID string) ([]BotSource, error) {
	if tenantID == "" {
		return nil, ErrTenantMissing
	}
	if ctxTenant, ok := store.TenantFromContext(ctx); ok && ctxTenant != "" && ctxTenant != tenantID {
		return nil, fmt.Errorf("botsource: tenant mismatch: ctx=%q arg=%q", ctxTenant, tenantID)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []BotSource
	for _, s := range m.byID {
		if s.TenantID == tenantID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
