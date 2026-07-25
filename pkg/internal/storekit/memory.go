// Package storekit holds the generic skeletons behind iterion's paired
// Mongo+memory store backends (pkg/audit, pkg/pat, pkg/cloudsched,
// pkg/webhooks, pkg/auth/desktopsso, pkg/auth/wsticket): a mutex-guarded
// keyed map (Memory), a typed Mongo collection wrapper composing on
// pkg/internal/mongoutil (Mongo), and a single-use TTL ticket pair
// (TicketMemory / TicketMongo). Domain-specific queries — filter
// vocabularies, CAS loops, quota counters, index geometry — stay in the
// domain packages; storekit only carries the shapes they all share, and
// surfaces failures through each package's own sentinels so callers keep
// comparing with errors.Is exactly as before.
package storekit

import "sync"

// Memory is a mutex-guarded map keyed by a caller-supplied string ID —
// the generic core of the in-process store backends used in tests and
// local mode. Misses are reported through the notFound sentinel the
// domain package supplies at construction, so its callers keep seeing
// their own ErrNotFound.
type Memory[T any] struct {
	mu       sync.RWMutex
	items    map[string]T
	notFound error
}

// NewMemory returns an empty store whose miss-shaped methods return
// notFound.
func NewMemory[T any](notFound error) *Memory[T] {
	return &Memory[T]{items: make(map[string]T), notFound: notFound}
}

// Insert stores v under id, returning dup when the id already exists.
func (m *Memory[T]) Insert(id string, v T, dup error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; ok {
		return dup
	}
	m.items[id] = v
	return nil
}

// InsertUnless stores v under id (overwriting an existing id), unless an
// existing item matches conflict — then it returns dup and writes
// nothing. The conflict scan and the write are one atomic step, which is
// what secondary-unique-key inserts (e.g. idempotency keys) need.
func (m *Memory[T]) InsertUnless(id string, v T, conflict func(T) bool, dup error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.items {
		if conflict(e) {
			return dup
		}
	}
	m.items[id] = v
	return nil
}

// Put upserts v under id.
func (m *Memory[T]) Put(id string, v T) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[id] = v
}

// Get returns the item stored under id, or notFound.
func (m *Memory[T]) Get(id string) (T, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.items[id]
	if !ok {
		return v, m.notFound
	}
	return v, nil
}

// Find returns an item matching match, or notFound. With multiple
// matches the pick is arbitrary (map order) — callers use it for
// unique secondary keys.
func (m *Memory[T]) Find(match func(T) bool) (T, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.items {
		if match(v) {
			return v, nil
		}
	}
	var zero T
	return zero, m.notFound
}

// List returns a snapshot of every item matching match, in arbitrary
// order — the domain caller sorts and bounds it.
func (m *Memory[T]) List(match func(T) bool) []T {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []T
	for _, v := range m.items {
		if match(v) {
			out = append(out, v)
		}
	}
	return out
}

// Replace overwrites the item stored under id, or returns notFound when
// id is absent (the checked-update shape).
func (m *Memory[T]) Replace(id string, v T) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return m.notFound
	}
	m.items[id] = v
	return nil
}

// Mutate applies fn to the item stored under id, writing the result back
// only when fn returns true. It reports (committed, notFound-when-absent),
// so it covers both checked field updates (fn always commits, caller
// propagates err) and compare-and-set (fn aborts, caller reads committed).
func (m *Memory[T]) Mutate(id string, fn func(*T) bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[id]
	if !ok {
		return false, m.notFound
	}
	if !fn(&v) {
		return false, nil
	}
	m.items[id] = v
	return true, nil
}

// Delete removes the item stored under id, or returns notFound.
func (m *Memory[T]) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return m.notFound
	}
	delete(m.items, id)
	return nil
}

// DeleteWhere removes every item matching match; removing nothing is not
// an error.
func (m *Memory[T]) DeleteWhere(match func(T) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, v := range m.items {
		if match(v) {
			delete(m.items, id)
		}
	}
}
