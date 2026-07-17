package configshare

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-memory Store for desktop/local mode and tests. Cloud
// uses the Mongo store (multi-replica; survives ephemeral runner pods).
type MemoryStore struct {
	mu         sync.RWMutex
	shares     map[string]*Share
	deliveries []*Delivery
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{shares: map[string]*Share{}} }

func (m *MemoryStore) Create(_ context.Context, s *Share) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.shares[s.ID]; ok {
		return fmt.Errorf("configshare: share %q already exists", s.ID)
	}
	cp := *s
	m.shares[s.ID] = &cp
	return nil
}

func (m *MemoryStore) GetByID(_ context.Context, id string) (*Share, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.shares[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (m *MemoryStore) ListByTenant(_ context.Context, tenantID string) ([]*Share, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Share
	for _, s := range m.shares {
		if s.TenantID == tenantID {
			cp := *s
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryStore) Update(_ context.Context, s *Share) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.shares[s.ID]; !ok {
		return ErrNotFound
	}
	cp := *s
	m.shares[s.ID] = &cp
	return nil
}

func (m *MemoryStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.shares[id]; !ok {
		return ErrNotFound
	}
	delete(m.shares, id)
	return nil
}

func (m *MemoryStore) Touch(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.shares[id]
	if !ok {
		return ErrNotFound
	}
	t := at
	s.LastUsedAt = &t
	return nil
}

func (m *MemoryStore) RecordDelivery(_ context.Context, d *Delivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *d
	m.deliveries = append(m.deliveries, &cp)
	return nil
}

func (m *MemoryStore) ListDeliveries(_ context.Context, shareID string, limit int) ([]*Delivery, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Delivery
	for i := len(m.deliveries) - 1; i >= 0; i-- {
		if m.deliveries[i].ShareID == shareID {
			cp := *m.deliveries[i]
			out = append(out, &cp)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
