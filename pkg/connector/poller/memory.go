package poller

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryStore is the in-process Store: the test twin, and the state the file
// store loads, mutates and writes back under its lock.
type MemoryStore struct {
	mu      sync.Mutex
	pollers map[string]*Poller
	seen    map[string]*seenRow
}

// seenRow is one key of a poller's seen set.
type seenRow struct {
	TenantID       string    `json:"tenant_id" bson:"tenant_id"`
	PollerID       string    `json:"poller_id" bson:"poller_id"`
	Key            ItemKey   `json:"key" bson:",inline"`
	FirstSeenAt    time.Time `json:"first_seen_at" bson:"first_seen_at"`
	LastObservedAt time.Time `json:"last_observed_at" bson:"last_observed_at"`
}

// memoryState is the store's serialized form (the file store's journal).
type memoryState struct {
	Pollers map[string]*Poller  `json:"pollers"`
	Seen    map[string]*seenRow `json:"seen"`
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{pollers: map[string]*Poller{}, seen: map[string]*seenRow{}}
}

func (m *MemoryStore) Ensure(_ context.Context, p Poller) (Poller, bool, error) {
	p, err := preparePoller(p)
	if err != nil {
		return Poller{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.pollers[p.ID]; ok {
		return copyPoller(*cur), false, nil
	}
	stored := copyPoller(p)
	m.pollers[p.ID] = &stored
	return copyPoller(stored), true, nil
}

func (m *MemoryStore) Get(_ context.Context, tenantID, id string) (Poller, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pollers[id]
	if !ok || p.TenantID != tenantID {
		return Poller{}, fmt.Errorf("%w: poller %s", ErrNotFound, id)
	}
	return copyPoller(*p), nil
}

func (m *MemoryStore) List(_ context.Context, tenantID string) ([]Poller, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Poller
	for _, p := range m.pollers {
		if p.TenantID == tenantID {
			out = append(out, copyPoller(*p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) ListDue(_ context.Context, now time.Time, limit int) ([]Poller, error) {
	now = stamp(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Poller
	for _, p := range m.pollers {
		if !p.NextTickAt.After(now) {
			out = append(out, copyPoller(*p))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].NextTickAt.Equal(out[j].NextTickAt) {
			return out[i].NextTickAt.Before(out[j].NextTickAt)
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// fenced answers the poller a fenced write may change, or why it may not.
func (m *MemoryStore) fenced(id string, generation int64) (*Poller, error) {
	p, ok := m.pollers[id]
	if !ok {
		return nil, fmt.Errorf("%w: poller %s", ErrNotFound, id)
	}
	if p.Generation != generation {
		return nil, fmt.Errorf("%w: poller %s is at generation %d, the write names %d", ErrFenced, id, p.Generation, generation)
	}
	return p, nil
}

func (m *MemoryStore) Acquire(_ context.Context, id string, generation int64, owner string, now time.Time) (Poller, error) {
	if err := checkOwner(owner); err != nil {
		return Poller{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.fenced(id, generation)
	if err != nil {
		return Poller{}, err
	}
	p.Generation++
	p.Holder = owner
	p.UpdatedAt = stamp(now)
	return copyPoller(*p), nil
}

func (m *MemoryStore) Wake(_ context.Context, tenantID, id string, at time.Time) error {
	at = stamp(at)
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pollers[id]
	if !ok || p.TenantID != tenantID {
		return fmt.Errorf("%w: poller %s", ErrNotFound, id)
	}
	if p.NextTickAt.After(at) {
		p.NextTickAt = at
	}
	return nil
}

func (m *MemoryStore) Reserve(_ context.Context, id string, generation int64, n int) (int64, error) {
	if n <= 0 {
		return 0, fmt.Errorf("%w: reserve %d sequence numbers", ErrInvalid, n)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.fenced(id, generation)
	if err != nil {
		return 0, err
	}
	first := p.Sequence + 1
	p.Sequence += int64(n)
	return first, nil
}

func (m *MemoryStore) Complete(_ context.Context, id string, generation int64, t Tick) error {
	t, err := checkTick(t)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.fenced(id, generation)
	if err != nil {
		return err
	}
	if behind(p.Checkpoint, t.Checkpoint) {
		return fmt.Errorf("%w: poller %s holds %s, the tick reached %s", ErrCheckpointBehind, id, p.Checkpoint.At.Format(time.RFC3339Nano), t.Checkpoint.At.Format(time.RFC3339Nano))
	}
	applyTick(p, t)
	return nil
}

// applyTick writes a checked tick onto a poller.
func applyTick(p *Poller, t Tick) {
	if t.Checkpoint != nil {
		c := copyCheckpoint(t.Checkpoint)
		p.Checkpoint = c
	}
	switch {
	case t.Idle:
		p.Primed = false
	case t.Primed:
		p.Primed, p.PrimedThrough = true, max(p.PrimedThrough, t.PrimedThrough)
	}
	at := t.At
	p.LastTickAt = &at
	p.NextTickAt = t.NextTickAt
	p.LastError = t.Error
	p.UpdatedAt = t.At
}

func (m *MemoryStore) Seen(_ context.Context, pollerID string, keys []ItemKey) (map[ItemKey]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[ItemKey]bool{}
	for _, k := range keys {
		if _, ok := m.seen[seenID(pollerID, k)]; ok {
			out[k] = true
		}
	}
	return out, nil
}

func (m *MemoryStore) MarkSeen(_ context.Context, tenantID, pollerID string, keys []ItemKey, at time.Time) error {
	if tenantID == "" || pollerID == "" || at.IsZero() {
		return fmt.Errorf("%w: mark seen without a tenant, a poller or an instant", ErrInvalid)
	}
	at = stamp(at)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		id := seenID(pollerID, k)
		if row, ok := m.seen[id]; ok {
			if at.After(row.LastObservedAt) {
				row.LastObservedAt = at
			}
			continue
		}
		m.seen[id] = &seenRow{TenantID: tenantID, PollerID: pollerID, Key: k, FirstSeenAt: at, LastObservedAt: at}
	}
	return nil
}

func (m *MemoryStore) PruneSeen(_ context.Context, pollerID string, before time.Time) (int, error) {
	before = stamp(before)
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, row := range m.seen {
		if row.PollerID == pollerID && row.LastObservedAt.Before(before) {
			delete(m.seen, id)
			n++
		}
	}
	return n, nil
}

func copyPoller(p Poller) Poller {
	if p.Query != nil {
		q := make(map[string]string, len(p.Query))
		for k, v := range p.Query {
			q[k] = v
		}
		p.Query = q
	}
	p.Checkpoint = copyCheckpoint(p.Checkpoint)
	if p.LastTickAt != nil {
		t := *p.LastTickAt
		p.LastTickAt = &t
	}
	return p
}

func copyCheckpoint(c *Checkpoint) *Checkpoint {
	if c == nil {
		return nil
	}
	out := *c
	if c.At != nil {
		t := *c.At
		out.At = &t
	}
	return &out
}
