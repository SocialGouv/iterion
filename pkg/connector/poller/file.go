package poller

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

// FileStore is the local durable twin: one JSON journal under the run store
// (.iterion/), reloaded under a file lock by every operation and replaced
// atomically before the operation answers. A refused write — a fenced one, a
// checkpoint behind — leaves the journal as it was.
type FileStore struct {
	mu   sync.Mutex
	path string
	lock *flock.Flock
}

// NewFileStore opens the journal at path; the file is created on first write.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path, lock: flock.New(path + ".lock")}
}

// withState loads the journal, runs fn on it, and writes it back when fn
// changed it and did not fail.
func (f *FileStore) withState(ctx context.Context, fn func(*MemoryStore) error) (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("poller: journal directory: %w", err)
	}
	ok, err := f.lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return fmt.Errorf("poller: lock journal %s: %w", f.path, err)
	}
	if !ok {
		return fmt.Errorf("poller: lock journal %s: %w", f.path, ctx.Err())
	}
	defer func() { err = errors.Join(err, f.lock.Unlock()) }()
	m := NewMemoryStore()
	raw, err := os.ReadFile(f.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("poller: read journal %s: %w", f.path, err)
	}
	if len(raw) > 0 {
		var st memoryState
		if err := json.Unmarshal(raw, &st); err != nil {
			return fmt.Errorf("poller: decode journal %s: %w", f.path, err)
		}
		if st.Pollers != nil {
			m.pollers = st.Pollers
		}
		if st.Seen != nil {
			m.seen = st.Seen
		}
	}
	if err := fn(m); err != nil {
		return err
	}
	next, err := json.Marshal(memoryState{Pollers: m.pollers, Seen: m.seen})
	if err != nil {
		return fmt.Errorf("poller: encode journal: %w", err)
	}
	if bytes.Equal(raw, next) {
		return nil
	}
	if err := store.WriteFileAtomic(f.path, next, 0o600); err != nil {
		return fmt.Errorf("poller: write journal %s: %w", f.path, err)
	}
	return nil
}

func (f *FileStore) Ensure(ctx context.Context, p Poller) (out Poller, created bool, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, created, e = m.Ensure(ctx, p)
		return e
	})
	return out, created, err
}

func (f *FileStore) Get(ctx context.Context, tenantID, id string) (out Poller, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.Get(ctx, tenantID, id)
		return e
	})
	return out, err
}

func (f *FileStore) List(ctx context.Context, tenantID string) (out []Poller, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.List(ctx, tenantID)
		return e
	})
	return out, err
}

func (f *FileStore) ListDue(ctx context.Context, now time.Time, limit int) (out []Poller, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.ListDue(ctx, now, limit)
		return e
	})
	return out, err
}

func (f *FileStore) Acquire(ctx context.Context, id string, generation int64, owner string, now time.Time) (out Poller, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.Acquire(ctx, id, generation, owner, now)
		return e
	})
	return out, err
}

func (f *FileStore) Reserve(ctx context.Context, id string, generation int64, n int) (first int64, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		first, e = m.Reserve(ctx, id, generation, n)
		return e
	})
	return first, err
}

func (f *FileStore) Complete(ctx context.Context, id string, generation int64, t Tick) error {
	return f.withState(ctx, func(m *MemoryStore) error {
		return m.Complete(ctx, id, generation, t)
	})
}

func (f *FileStore) Seen(ctx context.Context, pollerID string, keys []ItemKey) (out map[ItemKey]bool, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.Seen(ctx, pollerID, keys)
		return e
	})
	return out, err
}

func (f *FileStore) MarkSeen(ctx context.Context, tenantID, pollerID string, keys []ItemKey, at time.Time) error {
	return f.withState(ctx, func(m *MemoryStore) error {
		return m.MarkSeen(ctx, tenantID, pollerID, keys, at)
	})
}

func (f *FileStore) Wake(ctx context.Context, tenantID, id string, at time.Time) error {
	return f.withState(ctx, func(m *MemoryStore) error {
		return m.Wake(ctx, tenantID, id, at)
	})
}

func (f *FileStore) PruneSeen(ctx context.Context, pollerID string, before time.Time) (n int, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		n, e = m.PruneSeen(ctx, pollerID, before)
		return e
	})
	return n, err
}
