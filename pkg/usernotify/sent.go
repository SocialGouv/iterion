package usernotify

import (
	"context"
	"sync"
	"time"
)

// SentStore records which notification episodes were dispatched, providing
// (a) first-writer-wins dedup between the live bus handler and the
// reconciliation sweep (and across server replicas running the sweep), and
// (b) the "was this pause ever notified?" predicate the sweep scans for.
//
// The episode key is the run-outcome trigger.Event ID
// ("run:<id>:<interaction|status>"), distinct per pause/outcome episode.
type SentStore interface {
	// TryMark atomically claims key. It returns true when this caller won
	// the claim (proceed to deliver) and false when the episode was already
	// claimed.
	TryMark(ctx context.Context, key string) (bool, error)
	// Unmark releases a claim after a delivery that failed on every sink,
	// so the reconciliation sweep retries the episode. Best-effort.
	Unmark(ctx context.Context, key string) error
	// IsMarked reports whether key is already claimed WITHOUT claiming it —
	// the sweep's cheap pre-check that skips the per-run store load for
	// episodes long since delivered.
	IsMarked(ctx context.Context, key string) (bool, error)
}

// SentRecord is the persisted shape (exported for the Mongo store's TTL
// index; the mem store keeps only the key set).
type SentRecord struct {
	Key    string    `bson:"_id"`
	SentAt time.Time `bson:"sent_at"`
}

// MemSentStore is the in-memory SentStore for tests and local mode.
type MemSentStore struct {
	mu   sync.Mutex
	keys map[string]struct{}
}

func NewMemSentStore() *MemSentStore { return &MemSentStore{keys: make(map[string]struct{})} }

func (m *MemSentStore) TryMark(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.keys[key]; ok {
		return false, nil
	}
	m.keys[key] = struct{}{}
	return true, nil
}

func (m *MemSentStore) Unmark(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, key)
	return nil
}

func (m *MemSentStore) IsMarked(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.keys[key]
	return ok, nil
}
