package intake

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
// atomically before the operation answers. A process that restarts — or a
// second one sharing the directory — resumes from what the last answered
// operation wrote: pending work, dead letters, and claims a dead process left
// behind, which expire like any other.
type FileStore struct {
	mu   sync.Mutex
	path string
	lock *flock.Flock
	// now is handed to the loaded state as its expiry clock; nil reads the
	// wall clock.
	now func() time.Time
}

// NewFileStore opens the journal at path; the file is created on first write.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path, lock: flock.New(path + ".lock")}
}

// withState loads the journal, runs fn on it, and writes it back when fn
// changed it.
func (f *FileStore) withState(ctx context.Context, fn func(*MemoryStore) error) (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("intake: journal directory: %w", err)
	}
	ok, err := f.lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return fmt.Errorf("intake: lock journal %s: %w", f.path, err)
	}
	if !ok {
		return fmt.Errorf("intake: lock journal %s: %w", f.path, ctx.Err())
	}
	defer func() { err = errors.Join(err, f.lock.Unlock()) }()
	m := NewMemoryStore()
	m.now = f.now
	raw, err := os.ReadFile(f.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("intake: read journal %s: %w", f.path, err)
	}
	if len(raw) > 0 {
		var st memoryState
		if err := json.Unmarshal(raw, &st); err != nil {
			return fmt.Errorf("intake: decode journal %s: %w", f.path, err)
		}
		if st.Intakes != nil {
			m.intakes = st.Intakes
		}
		if st.Pairs != nil {
			m.pairs = st.Pairs
		}
	}
	if err := fn(m); err != nil {
		return err
	}
	next, err := json.Marshal(memoryState{Intakes: m.intakes, Pairs: m.pairs})
	if err != nil {
		return fmt.Errorf("intake: encode journal: %w", err)
	}
	if bytes.Equal(raw, next) {
		return nil
	}
	if err := store.WriteFileAtomic(f.path, next, 0o600); err != nil {
		return fmt.Errorf("intake: write journal %s: %w", f.path, err)
	}
	return nil
}

func (f *FileStore) Accept(ctx context.Context, in Intake) (out Intake, created bool, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, created, e = m.Accept(ctx, in)
		return e
	})
	return out, created, err
}

func (f *FileStore) GetIntake(ctx context.Context, tenantID, id string) (out Intake, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.GetIntake(ctx, tenantID, id)
		return e
	})
	return out, err
}

func (f *FileStore) CountBacklog(ctx context.Context, tenantID string) (n int, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		n, e = m.CountBacklog(ctx, tenantID)
		return e
	})
	return n, err
}

func (f *FileStore) ListIntakes(ctx context.Context, tenantID, state string, limit int) (out []Intake, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.ListIntakes(ctx, tenantID, state, limit)
		return e
	})
	return out, err
}

func (f *FileStore) ListPollerIntakes(ctx context.Context, tenantID, pollerID string, maxSeq int64, limit int) (out []Intake, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.ListPollerIntakes(ctx, tenantID, pollerID, maxSeq, limit)
		return e
	})
	return out, err
}

func (f *FileStore) ClaimIntakes(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) (out []Intake, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.ClaimIntakes(ctx, owner, now, ttl, limit)
		return e
	})
	return out, err
}

func (f *FileStore) MarkIntakeMatched(ctx context.Context, id string, generation int64, at time.Time) error {
	return f.withState(ctx, func(m *MemoryStore) error { return m.MarkIntakeMatched(ctx, id, generation, at) })
}

func (f *FileStore) RetryIntake(ctx context.Context, id string, generation int64, attempts int, notBefore time.Time, lastErr string) error {
	return f.withState(ctx, func(m *MemoryStore) error {
		return m.RetryIntake(ctx, id, generation, attempts, notBefore, lastErr)
	})
}

func (f *FileStore) BuryIntake(ctx context.Context, id string, generation int64, attempts int, lastErr string) error {
	return f.withState(ctx, func(m *MemoryStore) error { return m.BuryIntake(ctx, id, generation, attempts, lastErr) })
}

func (f *FileStore) ReplayIntake(ctx context.Context, tenantID, id string, now time.Time) (out Intake, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.ReplayIntake(ctx, tenantID, id, now)
		return e
	})
	return out, err
}

func (f *FileStore) WritePairs(ctx context.Context, pairs []Pair) error {
	return f.withState(ctx, func(m *MemoryStore) error { return m.WritePairs(ctx, pairs) })
}

func (f *FileStore) GetPair(ctx context.Context, tenantID, id string) (out Pair, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.GetPair(ctx, tenantID, id)
		return e
	})
	return out, err
}

func (f *FileStore) ListPairs(ctx context.Context, tenantID string, q PairQuery, limit int) (out []Pair, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.ListPairs(ctx, tenantID, q, limit)
		return e
	})
	return out, err
}

func (f *FileStore) LaunchedPair(ctx context.Context, tenantID, subscriptionID, itemKey string) (out Pair, found bool, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, found, e = m.LaunchedPair(ctx, tenantID, subscriptionID, itemKey)
		return e
	})
	return out, found, err
}

func (f *FileStore) ClaimPairs(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) (out []Pair, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.ClaimPairs(ctx, owner, now, ttl, limit)
		return e
	})
	return out, err
}

func (f *FileStore) MarkPairLaunched(ctx context.Context, id string, generation int64, runID string, at time.Time) error {
	return f.withState(ctx, func(m *MemoryStore) error { return m.MarkPairLaunched(ctx, id, generation, runID, at) })
}

func (f *FileStore) RetryPair(ctx context.Context, id string, generation int64, r PairRetry) error {
	return f.withState(ctx, func(m *MemoryStore) error { return m.RetryPair(ctx, id, generation, r) })
}

func (f *FileStore) RefusePair(ctx context.Context, id string, generation int64, reason string) error {
	return f.withState(ctx, func(m *MemoryStore) error { return m.RefusePair(ctx, id, generation, reason) })
}

func (f *FileStore) BuryPair(ctx context.Context, id string, generation int64, attempts int, lastErr string) error {
	return f.withState(ctx, func(m *MemoryStore) error { return m.BuryPair(ctx, id, generation, attempts, lastErr) })
}

func (f *FileStore) RejudgePair(ctx context.Context, tenantID, id string, v Verdict, now time.Time) (out Pair, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.RejudgePair(ctx, tenantID, id, v, now)
		return e
	})
	return out, err
}

func (f *FileStore) ReplayPair(ctx context.Context, tenantID, id string, now time.Time) (out Pair, err error) {
	err = f.withState(ctx, func(m *MemoryStore) error {
		var e error
		out, e = m.ReplayPair(ctx, tenantID, id, now)
		return e
	})
	return out, err
}

var _ Store = (*FileStore)(nil)
