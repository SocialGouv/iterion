package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/SocialGouv/iterion/pkg/store"
)

// FileSubscriptionStore is the local durable twin of the subscription store:
// one JSON journal under the run store (.iterion/), reloaded under a file
// lock by every operation and replaced atomically before a write answers. A
// restarted process — or a second one sharing the directory, `iterion
// dispatch` beside the studio — reads what the last answered write left:
// operator subscriptions, their edits and pins, and the tombstones of the
// derived rows an operator deleted.
type FileSubscriptionStore struct {
	mu   sync.Mutex
	path string
	lock *flock.Flock
}

// NewFileSubscriptionStore opens the journal at path; the file is created
// on the first write.
func NewFileSubscriptionStore(path string) *FileSubscriptionStore {
	return &FileSubscriptionStore{path: path, lock: flock.New(path + ".lock")}
}

// SubscriptionJournalPath is where a local server keeps its subscriptions,
// beside the effect outbox under the dispatcher directory.
func SubscriptionJournalPath(storeDir string) string {
	return filepath.Join(storeDir, "dispatcher", "trigger-subscriptions.json")
}

// fileSubscriptionRow carries the row's version, which the API view hides
// (Subscription.Version is json:"-") and the journal must keep for the CAS.
type fileSubscriptionRow struct {
	Subscription
	StoredVersion int64 `json:"version"`
}

type fileSubscriptionJournal struct {
	Subscriptions map[string]fileSubscriptionRow `json:"subscriptions,omitempty"`
	Tombstones    map[string]Tombstone           `json:"tombstones,omitempty"`
}

// withState loads the journal into a memory twin, runs fn on it, and writes
// the journal back when fn changed it. The journal is rewritten WHOLE, and
// unknown members decode away: two builds sharing one store directory drop
// each other's fields on their next write — run one build at a time. A
// caller classifies the error with errors.Is (the unlock's own error joins
// every return), never with ==.
func (f *FileSubscriptionStore) withState(ctx context.Context, fn func(*MemorySubscriptionStore) error) (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("trigger: subscription journal directory: %w", err)
	}
	ok, err := f.lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return fmt.Errorf("trigger: lock subscription journal %s: %w", f.path, err)
	}
	if !ok {
		return fmt.Errorf("trigger: lock subscription journal %s: %w", f.path, ctx.Err())
	}
	defer func() { err = errors.Join(err, f.lock.Unlock()) }()
	m := NewMemorySubscriptionStore()
	raw, err := os.ReadFile(f.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("trigger: read subscription journal %s: %w", f.path, err)
	}
	if len(raw) > 0 {
		var j fileSubscriptionJournal
		if err := json.Unmarshal(raw, &j); err != nil {
			return fmt.Errorf("trigger: decode subscription journal %s: %w", f.path, err)
		}
		for id, row := range j.Subscriptions {
			s := row.Subscription
			s.Version = row.StoredVersion
			m.items[id] = s
		}
		for id, t := range j.Tombstones {
			m.tombstones[id] = t
		}
	}
	if err := fn(m); err != nil {
		return err
	}
	j := fileSubscriptionJournal{
		Subscriptions: make(map[string]fileSubscriptionRow, len(m.items)),
		Tombstones:    m.tombstones,
	}
	for id, s := range m.items {
		j.Subscriptions[id] = fileSubscriptionRow{Subscription: s, StoredVersion: s.Version}
	}
	next, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("trigger: encode subscription journal: %w", err)
	}
	if bytes.Equal(raw, next) {
		return nil
	}
	if err := store.WriteFileAtomic(f.path, next, 0o600); err != nil {
		return fmt.Errorf("trigger: write subscription journal %s: %w", f.path, err)
	}
	return nil
}

func (f *FileSubscriptionStore) Create(ctx context.Context, s Subscription) error {
	return f.withState(ctx, func(m *MemorySubscriptionStore) error { return m.Create(ctx, s) })
}

func (f *FileSubscriptionStore) Get(ctx context.Context, id string) (out Subscription, err error) {
	err = f.withState(ctx, func(m *MemorySubscriptionStore) error {
		out, err = m.Get(ctx, id)
		return err
	})
	return out, err
}

func (f *FileSubscriptionStore) Update(ctx context.Context, s Subscription) error {
	return f.withState(ctx, func(m *MemorySubscriptionStore) error { return m.Update(ctx, s) })
}

func (f *FileSubscriptionStore) Delete(ctx context.Context, id string) error {
	return f.withState(ctx, func(m *MemorySubscriptionStore) error { return m.Delete(ctx, id) })
}

func (f *FileSubscriptionStore) MarkLaunchError(ctx context.Context, id, lastError string, at time.Time) error {
	return f.withState(ctx, func(m *MemorySubscriptionStore) error { return m.MarkLaunchError(ctx, id, lastError, at) })
}

func (f *FileSubscriptionStore) MarkRunOutcome(ctx context.Context, id, runID, status, errMsg, errCode string, at time.Time) error {
	return f.withState(ctx, func(m *MemorySubscriptionStore) error {
		return m.MarkRunOutcome(ctx, id, runID, status, errMsg, errCode, at)
	})
}

func (f *FileSubscriptionStore) ClearCredentialPins(ctx context.Context, id, recordID string) (cleared bool, err error) {
	err = f.withState(ctx, func(m *MemorySubscriptionStore) error {
		cleared, err = m.ClearCredentialPins(ctx, id, recordID)
		return err
	})
	return cleared, err
}

func (f *FileSubscriptionStore) EnsureDerived(ctx context.Context, s Subscription) (created bool, err error) {
	err = f.withState(ctx, func(m *MemorySubscriptionStore) error {
		created, err = m.EnsureDerived(ctx, s)
		return err
	})
	return created, err
}

func (f *FileSubscriptionStore) Tombstone(ctx context.Context, t Tombstone) error {
	return f.withState(ctx, func(m *MemorySubscriptionStore) error { return m.Tombstone(ctx, t) })
}

func (f *FileSubscriptionStore) ClearTombstone(ctx context.Context, tenantID, id string) error {
	return f.withState(ctx, func(m *MemorySubscriptionStore) error { return m.ClearTombstone(ctx, tenantID, id) })
}

// list runs one of the memory twin's listings over the journal as it stands.
func (f *FileSubscriptionStore) list(ctx context.Context, read func(*MemorySubscriptionStore) ([]Subscription, error)) (out []Subscription, err error) {
	err = f.withState(ctx, func(m *MemorySubscriptionStore) error {
		out, err = read(m)
		return err
	})
	return out, err
}

func (f *FileSubscriptionStore) ListByTenant(ctx context.Context, tenantID string) ([]Subscription, error) {
	return f.list(ctx, func(m *MemorySubscriptionStore) ([]Subscription, error) { return m.ListByTenant(ctx, tenantID) })
}

func (f *FileSubscriptionStore) ListByRepo(ctx context.Context, tenantID, repo string) ([]Subscription, error) {
	return f.list(ctx, func(m *MemorySubscriptionStore) ([]Subscription, error) { return m.ListByRepo(ctx, tenantID, repo) })
}

func (f *FileSubscriptionStore) ListByBot(ctx context.Context, tenantID, botID string) ([]Subscription, error) {
	return f.list(ctx, func(m *MemorySubscriptionStore) ([]Subscription, error) { return m.ListByBot(ctx, tenantID, botID) })
}

func (f *FileSubscriptionStore) ListByOrigin(ctx context.Context, tenantID, origin string) ([]Subscription, error) {
	return f.list(ctx, func(m *MemorySubscriptionStore) ([]Subscription, error) { return m.ListByOrigin(ctx, tenantID, origin) })
}

func (f *FileSubscriptionStore) ListCandidates(ctx context.Context, ev Event) ([]Subscription, error) {
	return f.list(ctx, func(m *MemorySubscriptionStore) ([]Subscription, error) { return m.ListCandidates(ctx, ev) })
}
