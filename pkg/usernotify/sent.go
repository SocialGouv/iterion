package usernotify

import (
	"context"
	"sync"
	"time"
)

// SentStore records which notification episodes were dispatched, providing
// (a) first-writer-wins dedup between the live bus handler and the
// reconciliation sweep (and across server replicas running the sweep), and
// (b) the "was this episode ever delivered?" predicate the sweep scans for.
//
// A claim has two phases: TryMark writes a PENDING claim before delivery,
// MarkDelivered confirms it once at least one sink succeeded. The split is
// what makes a crash mid-delivery recoverable: a pending claim older than
// ClaimGrace is stale (its owner died between claim and confirm) and
// TryMark takes it over, so the episode is retried instead of being lost
// forever behind a claim nobody honoured.
//
// The episode key is the run-outcome trigger.Event ID
// ("run:<id>:<interaction|status>:<updated-at>"), distinct per
// pause/outcome episode.
type SentStore interface {
	// TryMark atomically claims key. It returns true when this caller won
	// the claim (proceed to deliver) — either the first claim on the
	// episode, or a takeover of a stale pending claim.
	TryMark(ctx context.Context, key string) (bool, error)
	// MarkDelivered confirms the claim after a successful delivery (or a
	// deliberate no-op — nothing addressable). Confirmed episodes are
	// never retried.
	MarkDelivered(ctx context.Context, key string) error
	// Unmark releases a claim after a delivery that failed on every sink,
	// so the reconciliation sweep retries the episode promptly.
	// Best-effort (a lost Unmark degrades to the ClaimGrace takeover).
	Unmark(ctx context.Context, key string) error
	// IsMarked reports whether key is settled — delivered, or pending
	// within ClaimGrace (someone is actively on it) — WITHOUT claiming
	// it. The sweep's cheap pre-check that skips the per-run store load.
	IsMarked(ctx context.Context, key string) (bool, error)
	// WasDelivered reports whether key was CONFIRMED delivered — strictly
	// narrower than IsMarked (a pending claim inside ClaimGrace is marked
	// but not delivered). Consumers that certify side-state off another
	// claim's outcome (the ops dispatcher's transition markers) must read
	// this, never IsMarked: certifying a pending claim races its Unmark.
	WasDelivered(ctx context.Context, key string) (bool, error)
}

// ClaimGrace is how long a pending (unconfirmed) claim shields its episode
// before being treated as abandoned. Delivery takes seconds; the grace
// covers a slow fan-out plus clock skew between replicas without making a
// crashed pod's episode wait long.
const ClaimGrace = 10 * time.Minute

// SentRecord is the persisted claim (exported for the Mongo store's TTL
// index shape).
type SentRecord struct {
	Key       string    `bson:"_id"`
	SentAt    time.Time `bson:"sent_at"`
	Delivered bool      `bson:"delivered"`
}

// MemSentStore is the in-memory SentStore for tests and local mode.
type MemSentStore struct {
	mu   sync.Mutex
	keys map[string]SentRecord
	// now is the clock, swappable in tests to age a pending claim.
	now func() time.Time
}

func NewMemSentStore() *MemSentStore {
	return &MemSentStore{keys: make(map[string]SentRecord), now: time.Now}
}

func (m *MemSentStore) TryMark(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	if rec, ok := m.keys[key]; ok {
		if rec.Delivered || now.Sub(rec.SentAt) < ClaimGrace {
			return false, nil
		}
		// Stale pending claim: its owner died mid-delivery — take over.
	}
	m.keys[key] = SentRecord{Key: key, SentAt: now}
	return true, nil
}

func (m *MemSentStore) MarkDelivered(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[key] = SentRecord{Key: key, SentAt: m.now().UTC(), Delivered: true}
	return nil
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
	rec, ok := m.keys[key]
	if !ok {
		return false, nil
	}
	return rec.Delivered || m.now().UTC().Sub(rec.SentAt) < ClaimGrace, nil
}

func (m *MemSentStore) WasDelivered(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keys[key].Delivered, nil
}
