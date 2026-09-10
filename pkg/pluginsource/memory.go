package pluginsource

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore is an in-process Store for tests and local mode.
type MemoryStore struct {
	mu   sync.RWMutex
	byID map[string]PluginSource
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: make(map[string]PluginSource)}
}

func (m *MemoryStore) Create(_ context.Context, s PluginSource) error {
	if err := s.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// (tenant, name) is the identity an operator reasons about: two sources
	// with the same name would race for the same registry directory.
	for _, e := range m.byID {
		if e.TenantID == s.TenantID && e.Name == s.Name && e.ID != s.ID {
			return ErrNameConflict
		}
	}
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	s.CreatedAt, s.UpdatedAt = now, now
	m.byID[s.ID] = s
	return nil
}

func (m *MemoryStore) Get(_ context.Context, id string) (PluginSource, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.byID[id]
	if !ok {
		return PluginSource{}, ErrNotFound
	}
	return s, nil
}

func (m *MemoryStore) Update(_ context.Context, s PluginSource) error {
	if err := s.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, ok := m.byID[s.ID]
	if !ok {
		return ErrNotFound
	}
	for _, e := range m.byID {
		if e.TenantID == s.TenantID && e.Name == s.Name && e.ID != s.ID {
			return ErrNameConflict
		}
	}
	s.CreatedAt = prev.CreatedAt
	s.CreatedBy = prev.CreatedBy
	// Health is the engine's readout, written only by Mark/ClearDegraded.
	s.DegradedReason, s.DegradedAt = prev.DegradedReason, prev.DegradedAt
	s.UpdatedAt = time.Now().UTC()
	m.byID[s.ID] = s
	return nil
}

func (m *MemoryStore) MarkDegraded(_ context.Context, tenantID, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok || s.TenantID != tenantID {
		return ErrNotFound
	}
	now := time.Now().UTC()
	s.DegradedReason, s.DegradedAt = reason, &now
	m.byID[id] = s
	return nil
}

func (m *MemoryStore) ClearDegraded(_ context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	if !ok || s.TenantID != tenantID {
		return ErrNotFound
	}
	s.DegradedReason, s.DegradedAt = "", nil
	m.byID[id] = s
	return nil
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

func (m *MemoryStore) ListEnabledByTenant(ctx context.Context, tenantID string) ([]PluginSource, error) {
	all, err := m.ListByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]PluginSource, 0, len(all))
	for _, s := range all {
		if s.Enabled {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *MemoryStore) ListByTenant(_ context.Context, tenantID string) ([]PluginSource, error) {
	if tenantID == "" {
		return nil, ErrTenantMissing
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []PluginSource
	for _, s := range m.byID {
		if s.TenantID == tenantID {
			out = append(out, s)
		}
	}
	return out, nil
}
