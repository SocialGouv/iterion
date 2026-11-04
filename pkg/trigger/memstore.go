package trigger

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

// MemorySubscriptionStore is the in-memory SubscriptionStore for tests and
// local single-host runs. It loads from subscriptions.yaml + the dispatcher
// config + schedules.yaml at boot and is the store the InProcBus evaluator
// queries. Goroutine-safe; mirrors forge.MemoryRepoIntegrationStore.
//
// Every Subscription crossing a boundary is a storekit.Snapshot copy (the
// forge and webhooks twin discipline): readers own their maps and callers'
// payloads stay theirs, because the clear below mutates the stored pin maps
// IN PLACE under the write lock — a handed-out shared header would make any
// post-lock reader of it (a route's JSON marshal, the sweep's pinsName) race
// that delete into Go's unrecoverable concurrent map iteration and map
// write (#2644).
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
	m.items[s.ID] = storekit.Snapshot(s)
	return nil
}

func (m *MemorySubscriptionStore) Get(_ context.Context, id string) (Subscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.items[id]
	if !ok {
		return Subscription{}, ErrSubscriptionNotFound
	}
	return storekit.Snapshot(s), nil
}

func (m *MemorySubscriptionStore) Update(_ context.Context, s Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.items[s.ID]
	if !ok {
		return ErrSubscriptionNotFound
	}
	if cur.Version != s.Version {
		return ErrSubscriptionConflict
	}
	s.Version++
	// The health stamps are the marks' (MarkLaunchError, MarkRunOutcome):
	// targeted writes that bump no version — a keepalive firing every tick
	// must not conflict every operator edit — so a copy read before a fire
	// passes the CAS and would erase them. The stored stamps win, as the
	// webhooks twin keeps LastUsedAt.
	s.LastError, s.LastErrorAt = cur.LastError, cur.LastErrorAt
	s.LastRunID, s.LastRunStatus, s.LastRunError, s.LastRunErrorCode, s.LastRunAt =
		cur.LastRunID, cur.LastRunStatus, cur.LastRunError, cur.LastRunErrorCode, cur.LastRunAt
	m.items[s.ID] = storekit.Snapshot(s)
	return nil
}

func (m *MemorySubscriptionStore) MarkLaunchError(_ context.Context, id, lastError string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.items[id]
	if !ok {
		return ErrSubscriptionNotFound
	}
	s.LastError = lastError
	s.LastErrorAt = nil
	if lastError != "" {
		t := at.UTC()
		s.LastErrorAt = &t
	}
	m.items[id] = s
	return nil
}

func (m *MemorySubscriptionStore) MarkRunOutcome(_ context.Context, id, runID, status, errMsg, errCode string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.items[id]
	if !ok {
		return ErrSubscriptionNotFound
	}
	// Monotonicity guard (twin of the mongo filter): an out-of-order
	// delivery — overlapping runs of one subscription, or a redelivery
	// racing a newer stamp — cannot move the health fields backwards.
	if s.LastRunAt != nil && s.LastRunAt.After(at.UTC()) {
		return nil
	}
	s.LastRunID = runID
	s.LastRunStatus = status
	s.LastRunError = errMsg
	s.LastRunErrorCode = errCode
	t := at.UTC()
	s.LastRunAt = &t
	m.items[id] = s
	return nil
}

// ClearCredentialPins judges the row the store holds under the same lock as
// the write: the caller's listing copy names nothing here, so a health
// stamp, a pin edit, or a second record's sweep landing after that copy was
// taken survives untouched. Both maps are scanned unconditionally — the
// contract removes every entry naming the record, so the first map's drop
// never spares the second its judgment. Each real removal bumps the row's
// version, so a full-document editor holding a pre-sweep snapshot conflicts
// instead of writing the swept pin back.
func (m *MemorySubscriptionStore) ClearCredentialPins(_ context.Context, id, recordID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.items[id]
	if !ok {
		return false, ErrSubscriptionNotFound
	}
	droppedKeys := storekit.DropStringMapValues(&s.KeyOverrides, recordID)
	droppedSecrets := storekit.DropStringMapValues(&s.SecretOverrides, recordID)
	cleared := droppedKeys || droppedSecrets
	if cleared {
		s.Version++
		m.items[id] = s
	}
	return cleared, nil
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
	return storekit.Snapshot(out)
}
