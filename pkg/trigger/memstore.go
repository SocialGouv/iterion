package trigger

import (
	"context"
	"sort"
	"sync"
)

// MemorySubscriptionStore is the in-memory SubscriptionStore for tests and
// local single-host runs. It loads from subscriptions.yaml + the dispatcher
// config + schedules.yaml at boot and is the store the InProcBus evaluator
// queries. Goroutine-safe; mirrors forge.MemoryRepoIntegrationStore.
type MemorySubscriptionStore struct {
	mu    sync.RWMutex
	items map[string]Subscription
}

func NewMemorySubscriptionStore() *MemorySubscriptionStore {
	return &MemorySubscriptionStore{items: make(map[string]Subscription)}
}

func (m *MemorySubscriptionStore) Create(_ context.Context, s Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[s.ID] = s
	return nil
}

func (m *MemorySubscriptionStore) Get(_ context.Context, id string) (Subscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.items[id]
	if !ok {
		return Subscription{}, ErrSubscriptionNotFound
	}
	return s, nil
}

func (m *MemorySubscriptionStore) Update(_ context.Context, s Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[s.ID]; !ok {
		return ErrSubscriptionNotFound
	}
	m.items[s.ID] = s
	return nil
}

func (m *MemorySubscriptionStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return ErrSubscriptionNotFound
	}
	delete(m.items, id)
	return nil
}

func (m *MemorySubscriptionStore) ListByTenant(_ context.Context, tenantID string) ([]Subscription, error) {
	return m.filter(func(s Subscription) bool { return s.TenantID == tenantID }), nil
}

func (m *MemorySubscriptionStore) ListByRepo(_ context.Context, tenantID, repo string) ([]Subscription, error) {
	return m.filter(func(s Subscription) bool {
		return s.TenantID == tenantID && (s.Repo == repo || s.Repo == "")
	}), nil
}

func (m *MemorySubscriptionStore) ListByBot(_ context.Context, tenantID, botID string) ([]Subscription, error) {
	return m.filter(func(s Subscription) bool {
		return s.TenantID == tenantID && s.BotID == botID
	}), nil
}

func (m *MemorySubscriptionStore) ListByOrigin(_ context.Context, tenantID, origin string) ([]Subscription, error) {
	return m.filter(func(s Subscription) bool {
		return s.TenantID == tenantID && s.Origin == origin
	}), nil
}

func (m *MemorySubscriptionStore) ListCandidates(_ context.Context, ev Event) ([]Subscription, error) {
	return m.filter(func(s Subscription) bool {
		return s.Enabled && s.TenantID == ev.TenantID && (s.Repo == ev.Repo || s.Repo == "")
	}), nil
}

func (m *MemorySubscriptionStore) filter(keep func(Subscription) bool) []Subscription {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Subscription
	for _, s := range m.items {
		if keep(s) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}
