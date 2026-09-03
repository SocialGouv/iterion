package webhooks

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

// MemoryConfigStore is an in-process ConfigStore for tests and local
// mode. Keep its semantics in lock-step with MongoConfigStore.
type MemoryConfigStore struct {
	kit *storekit.Memory[Config]
}

func NewMemoryConfigStore() *MemoryConfigStore {
	return &MemoryConfigStore{kit: storekit.NewMemory[Config](ErrNotFound)}
}

func (s *MemoryConfigStore) Create(_ context.Context, c Config) error {
	return s.kit.Insert(c.ID, c, ErrDuplicate)
}

func (s *MemoryConfigStore) Get(_ context.Context, id string) (Config, error) {
	return s.kit.Get(id)
}

func (s *MemoryConfigStore) Update(_ context.Context, c Config) error {
	return s.kit.Replace(c.ID, c)
}

func (s *MemoryConfigStore) Delete(_ context.Context, id string) error {
	return s.kit.Delete(id)
}

func (s *MemoryConfigStore) ListByTenant(_ context.Context, tenantID string) ([]Config, error) {
	out := s.kit.List(func(c Config) bool { return c.TenantID == tenantID })
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryConfigStore) MarkUsed(_ context.Context, id string, t time.Time) error {
	_, err := s.kit.Mutate(id, func(c *Config) bool {
		c.LastUsedAt = &t
		return true
	})
	return err
}

// MemoryDeliveryStore is an in-process DeliveryStore.
type MemoryDeliveryStore struct {
	kit *storekit.Memory[Delivery]
}

func NewMemoryDeliveryStore() *MemoryDeliveryStore {
	return &MemoryDeliveryStore{kit: storekit.NewMemory[Delivery](ErrNotFound)}
}

func (s *MemoryDeliveryStore) Insert(_ context.Context, d Delivery) error {
	// The idempotency key is the unique constraint (when present) — the
	// durable dedupe MongoDeliveryStore enforces with a unique index.
	return s.kit.InsertUnless(d.ID, d, func(e Delivery) bool {
		return d.IdempotencyKey != "" && e.IdempotencyKey == d.IdempotencyKey
	}, ErrDuplicate)
}

func (s *MemoryDeliveryStore) GetByIdempotencyKey(_ context.Context, key string) (Delivery, error) {
	return s.kit.Find(func(e Delivery) bool {
		return key != "" && e.IdempotencyKey == key
	})
}

func (s *MemoryDeliveryStore) Update(_ context.Context, d Delivery) error {
	return s.kit.Replace(d.ID, d)
}

func (s *MemoryDeliveryStore) ListByWebhook(_ context.Context, tenantID, webhookID string, limit int) ([]Delivery, error) {
	out := s.kit.List(func(d Delivery) bool {
		return d.TenantID == tenantID && d.WebhookID == webhookID
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ReceivedAt.After(out[j].ReceivedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryDeliveryStore) CountLaunched(_ context.Context, tenantID, webhookID, eventKind, projectPath, subjectID string) (int, error) {
	return len(s.kit.List(func(d Delivery) bool {
		return d.TenantID == tenantID && d.WebhookID == webhookID &&
			d.EventKind == eventKind && d.ProjectPath == projectPath &&
			d.SubjectID == subjectID && d.RunID != ""
	})), nil
}

// ListLaunchedBySubject returns the subject's launched deliveries.
func (s *MemoryDeliveryStore) ListLaunchedBySubject(_ context.Context, tenantID, webhookID, projectPath, subjectID string) ([]Delivery, error) {
	return s.kit.List(func(d Delivery) bool {
		return d.TenantID == tenantID && d.WebhookID == webhookID &&
			d.ProjectPath == projectPath && d.RunID != "" &&
			(d.SubjectID == subjectID || d.ParentSubjectID == subjectID)
	}), nil
}

// MemoryDeferredLaunchStore is an in-process DeferredLaunchStore. Keep
// its semantics in lock-step with MongoDeferredLaunchStore.
type MemoryDeferredLaunchStore struct {
	mu   sync.Mutex
	rows map[string]DeferredLaunch // by SubjectKey
}

func NewMemoryDeferredLaunchStore() *MemoryDeferredLaunchStore {
	return &MemoryDeferredLaunchStore{rows: make(map[string]DeferredLaunch)}
}

func (s *MemoryDeferredLaunchStore) Upsert(_ context.Context, d DeferredLaunch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Newest payload wins wholesale — including clearing any lease and any
	// retry budget the OLD payload had spent: a fresh push during a
	// claimed row's launch re-arms the subject from scratch.
	d.ClaimedUntil = time.Time{}
	d.Attempts = 0
	if prev, ok := s.rows[d.SubjectKey]; ok {
		d.Generation = prev.Generation + 1
		d.CreatedAt = prev.CreatedAt
	} else {
		d.Generation = 1
	}
	s.rows[d.SubjectKey] = d
	return nil
}

func (s *MemoryDeferredLaunchStore) ClaimDue(_ context.Context, now time.Time, lease time.Duration, limit int) ([]DeferredLaunch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []DeferredLaunch
	for k, d := range s.rows {
		if limit > 0 && len(out) >= limit {
			break
		}
		if d.FireAt.After(now) || d.ClaimedUntil.After(now) {
			continue
		}
		d.ClaimedUntil = now.Add(lease)
		s.rows[k] = d
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FireAt.Before(out[j].FireAt) })
	return out, nil
}

func (s *MemoryDeferredLaunchStore) Delete(_ context.Context, subjectKey string, generation int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.rows[subjectKey]; ok && d.Generation == generation {
		delete(s.rows, subjectKey)
	}
	return nil
}

func (s *MemoryDeferredLaunchStore) RescheduleFailed(_ context.Context, subjectKey string, generation int64, fireAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[subjectKey]
	if !ok || d.Generation != generation {
		// A fresher payload re-armed the subject mid-retry — it wins.
		return nil
	}
	d.Attempts++
	d.FireAt = fireAt
	d.ClaimedUntil = time.Time{}
	s.rows[subjectKey] = d
	return nil
}

// MemoryCounter is an in-process monthly Counter. Production uses the
// Mongo CAS variant; this one is mutex-serialised.
type MemoryCounter struct {
	mu  sync.Mutex
	org map[string]int // tenant|YYYY-MM -> count
	wh  map[string]int // tenant|webhook|YYYY-MM -> count
}

func NewMemoryCounter() *MemoryCounter {
	return &MemoryCounter{org: make(map[string]int), wh: make(map[string]int)}
}

func monthKey(when time.Time) string { return when.UTC().Format("2006-01") }

func (c *MemoryCounter) Allow(_ context.Context, tenantID, webhookID string, when time.Time, lim Limits) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := monthKey(when)
	ok := tenantID + "|" + m
	wk := tenantID + "|" + webhookID + "|" + m
	if lim.PerOrgMonthly > 0 && c.org[ok]+1 > lim.PerOrgMonthly {
		return false, nil
	}
	if lim.PerWebhookMonthly > 0 && c.wh[wk]+1 > lim.PerWebhookMonthly {
		return false, nil
	}
	c.org[ok]++
	c.wh[wk]++
	return true, nil
}

func (c *MemoryCounter) OrgCount(_ context.Context, tenantID string, when time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.org[tenantID+"|"+monthKey(when)], nil
}
