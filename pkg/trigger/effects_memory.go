package trigger

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryEffectOutbox is the in-process EffectOutbox — the test twin of the
// boardmongo implementation, mutex-serialised.
type MemoryEffectOutbox struct {
	mu   sync.Mutex
	rows map[string]*EffectRow
}

func NewMemoryEffectOutbox() *MemoryEffectOutbox {
	return &MemoryEffectOutbox{rows: map[string]*EffectRow{}}
}

func (m *MemoryEffectOutbox) UpsertPending(_ context.Context, rows []EffectRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rows {
		if _, ok := m.rows[r.ID]; ok {
			continue // $setOnInsert semantics: existing rows untouched
		}
		cp := r
		if cp.State == "" {
			cp.State = EffectPending
		}
		m.rows[cp.ID] = &cp
	}
	return nil
}

func (m *MemoryEffectOutbox) ClaimDue(_ context.Context, now time.Time, limit int) ([]EffectRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []*EffectRow
	for _, r := range m.rows {
		eligible := (r.State == EffectPending || r.State == EffectClaimed) && !r.NotBefore.After(now)
		if eligible {
			due = append(due, r)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].CreatedAt.Before(due[j].CreatedAt) })
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	out := make([]EffectRow, 0, len(due))
	for _, r := range due {
		r.State = EffectClaimed
		r.NotBefore = now.Add(EffectLease)
		r.UpdatedAt = now
		out = append(out, *r)
	}
	return out, nil
}

func (m *MemoryEffectOutbox) mutate(id string, fn func(*EffectRow)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok {
		fn(r)
		r.UpdatedAt = time.Now().UTC()
	}
	return nil
}

func (m *MemoryEffectOutbox) MarkConsumed(_ context.Context, id string) error {
	return m.mutate(id, func(r *EffectRow) { r.ConsumeMarked = true })
}

func (m *MemoryEffectOutbox) MarkDone(_ context.Context, id string) error {
	return m.mutate(id, func(r *EffectRow) { r.State = EffectDone })
}

func (m *MemoryEffectOutbox) MarkRetry(_ context.Context, id string, attempts int, notBefore time.Time, lastErr string) error {
	return m.mutate(id, func(r *EffectRow) {
		r.State = EffectPending
		r.Attempts = attempts
		r.NotBefore = notBefore
		r.LastError = lastErr
	})
}

func (m *MemoryEffectOutbox) MarkFailed(_ context.Context, id string, lastErr string) error {
	return m.mutate(id, func(r *EffectRow) {
		r.State = EffectFailed
		r.LastError = lastErr
	})
}

// Row returns a copy for test assertions.
func (m *MemoryEffectOutbox) Row(id string) (EffectRow, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok {
		return *r, true
	}
	return EffectRow{}, false
}

var _ EffectOutbox = (*MemoryEffectOutbox)(nil)
