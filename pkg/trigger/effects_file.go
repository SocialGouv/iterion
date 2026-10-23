package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/gofrs/flock"
)

// FileEffectOutbox is the local durable twin. Every operation reloads under a
// file lock, then atomically replaces the journal before acknowledging it.
// Reopening it after a process restart recovers pending work and expired claims.
type FileEffectOutbox struct {
	mu   sync.Mutex
	path string
	lock *flock.Flock
}

func NewFileEffectOutbox(path string) *FileEffectOutbox {
	return &FileEffectOutbox{path: path, lock: flock.New(path + ".lock")}
}
func (f *FileEffectOutbox) mutate(ctx context.Context, fn func(*MemoryEffectOutbox) error) (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.path), 0700); err != nil {
		return err
	}
	ok, err := f.lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ctx.Err()
	}
	defer func() { err = errors.Join(err, f.lock.Unlock()) }()
	m := NewMemoryEffectOutbox()
	raw, err := os.ReadFile(f.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m.rows); err != nil {
			return err
		}
	}
	if err := fn(m); err != nil {
		return err
	}
	before := raw
	raw, err = json.Marshal(m.rows)
	if err != nil {
		return err
	}
	if bytes.Equal(before, raw) {
		return nil
	}
	return store.WriteFileAtomic(f.path, raw, 0600)
}
func (f *FileEffectOutbox) UpsertPending(ctx context.Context, rows []EffectRow) error {
	return f.mutate(ctx, func(m *MemoryEffectOutbox) error { return m.UpsertPending(ctx, rows) })
}
func (f *FileEffectOutbox) ClaimDue(ctx context.Context, now time.Time, limit int) (rows []EffectRow, err error) {
	err = f.mutate(ctx, func(m *MemoryEffectOutbox) error { var err error; rows, err = m.ClaimDue(ctx, now, limit); return err })
	if err != nil {
		return nil, err
	}
	return rows, nil
}
func (f *FileEffectOutbox) MarkConsumed(ctx context.Context, id, claim string) error {
	return f.mutate(ctx, func(m *MemoryEffectOutbox) error { return m.MarkConsumed(ctx, id, claim) })
}
func (f *FileEffectOutbox) MarkDone(ctx context.Context, id, claim string) error {
	return f.mutate(ctx, func(m *MemoryEffectOutbox) error { return m.MarkDone(ctx, id, claim) })
}
func (f *FileEffectOutbox) MarkRetry(ctx context.Context, id, claim string, attempts int, at time.Time, msg string) error {
	return f.mutate(ctx, func(m *MemoryEffectOutbox) error { return m.MarkRetry(ctx, id, claim, attempts, at, msg) })
}
func (f *FileEffectOutbox) MarkFailed(ctx context.Context, id, claim, msg string) error {
	return f.mutate(ctx, func(m *MemoryEffectOutbox) error { return m.MarkFailed(ctx, id, claim, msg) })
}
