package cloudsched

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

// MemoryStore is an in-memory Store for tests + single-process use. ClaimTick
// enforces the same CAS-on-next_fire_at semantics as the Mongo store.
type MemoryStore struct {
	kit *storekit.Memory[ScheduledBot]
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{kit: storekit.NewMemory[ScheduledBot](ErrNotFound)}
}

var _ Store = (*MemoryStore)(nil)

func (s *MemoryStore) Create(_ context.Context, sb ScheduledBot) error {
	return s.kit.Insert(sb.ID, sb, fmt.Errorf("cloudsched: schedule %q already exists", sb.ID))
}

func (s *MemoryStore) Get(_ context.Context, id string) (ScheduledBot, error) {
	return s.kit.Get(id)
}

func (s *MemoryStore) ListByIntegration(_ context.Context, tenantID, integrationID string) ([]ScheduledBot, error) {
	return s.kit.List(func(sb ScheduledBot) bool {
		return sb.TenantID == tenantID && sb.RepoIntegrationID == integrationID
	}), nil
}

func (s *MemoryStore) ListByTenant(_ context.Context, tenantID string) ([]ScheduledBot, error) {
	out := s.kit.List(func(sb ScheduledBot) bool { return sb.TenantID == tenantID })
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryStore) ListDue(_ context.Context, now time.Time, limit int) ([]ScheduledBot, error) {
	out := s.kit.List(func(sb ScheduledBot) bool {
		return !sb.Disabled && !sb.NextFireAt.After(now)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].NextFireAt.Before(out[j].NextFireAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) ClaimTick(_ context.Context, id string, expectedNext, newNext, firedAt time.Time) (bool, error) {
	// A missing row and a lost CAS both mean "didn't win", never an error.
	won, _ := s.kit.Mutate(id, func(sb *ScheduledBot) bool {
		if !sb.NextFireAt.Equal(expectedNext) {
			return false // lost the CAS (another caller already advanced it)
		}
		sb.NextFireAt = newNext
		f := firedAt
		sb.LastFireAt = &f
		sb.UpdatedAt = firedAt
		return true
	})
	return won, nil
}

func (s *MemoryStore) Update(_ context.Context, id string, patch SchedulePatch) (ScheduledBot, error) {
	var out ScheduledBot
	if _, err := s.kit.Mutate(id, func(sb *ScheduledBot) bool {
		applySchedulePatch(sb, patch)
		out = *sb
		return true
	}); err != nil {
		return ScheduledBot{}, err
	}
	return out, nil
}

func (s *MemoryStore) Delete(_ context.Context, id string) error {
	return s.kit.Delete(id)
}

func (s *MemoryStore) DeleteByIntegration(_ context.Context, tenantID, integrationID string) error {
	s.kit.DeleteWhere(func(sb ScheduledBot) bool {
		return sb.TenantID == tenantID && sb.RepoIntegrationID == integrationID
	})
	return nil
}
