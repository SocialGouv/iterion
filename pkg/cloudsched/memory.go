package cloudsched

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-memory Store for tests + single-process use. ClaimTick
// enforces the same CAS-on-next_fire_at semantics as the Mongo store.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]ScheduledBot
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string]ScheduledBot{}} }

var _ Store = (*MemoryStore)(nil)

func (s *MemoryStore) Create(_ context.Context, sb ScheduledBot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[sb.ID]; ok {
		return fmt.Errorf("cloudsched: schedule %q already exists", sb.ID)
	}
	s.m[sb.ID] = sb
	return nil
}

func (s *MemoryStore) Get(_ context.Context, id string) (ScheduledBot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.m[id]
	if !ok {
		return ScheduledBot{}, ErrNotFound
	}
	return sb, nil
}

func (s *MemoryStore) ListByIntegration(_ context.Context, tenantID, integrationID string) ([]ScheduledBot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ScheduledBot
	for _, sb := range s.m {
		if sb.TenantID == tenantID && sb.RepoIntegrationID == integrationID {
			out = append(out, sb)
		}
	}
	return out, nil
}

func (s *MemoryStore) ListByTenant(_ context.Context, tenantID string) ([]ScheduledBot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ScheduledBot
	for _, sb := range s.m {
		if sb.TenantID == tenantID {
			out = append(out, sb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryStore) ListDue(_ context.Context, now time.Time, limit int) ([]ScheduledBot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ScheduledBot
	for _, sb := range s.m {
		if !sb.Disabled && !sb.NextFireAt.After(now) {
			out = append(out, sb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextFireAt.Before(out[j].NextFireAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) ClaimTick(_ context.Context, id string, expectedNext, newNext, firedAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.m[id]
	if !ok || !sb.NextFireAt.Equal(expectedNext) {
		return false, nil // lost the CAS (another caller already advanced it)
	}
	sb.NextFireAt = newNext
	f := firedAt
	sb.LastFireAt = &f
	sb.UpdatedAt = firedAt
	s.m[id] = sb
	return true, nil
}

func (s *MemoryStore) Update(_ context.Context, id string, patch SchedulePatch) (ScheduledBot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.m[id]
	if !ok {
		return ScheduledBot{}, ErrNotFound
	}
	applySchedulePatch(&sb, patch)
	s.m[id] = sb
	return sb, nil
}

func (s *MemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		return ErrNotFound
	}
	delete(s.m, id)
	return nil
}

func (s *MemoryStore) DeleteByIntegration(_ context.Context, tenantID, integrationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sb := range s.m {
		if sb.TenantID == tenantID && sb.RepoIntegrationID == integrationID {
			delete(s.m, id)
		}
	}
	return nil
}
