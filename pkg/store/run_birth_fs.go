package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type filesystemRunBirth struct {
	mu          sync.Mutex
	store       *FilesystemRunStore
	id          string
	format      ValueFormat
	stage       string
	preparation RunLock
	execution   RunLock
	used        bool
	published   bool
	released    bool
	syncDir     func(string) error
	releaseErr  error
}

var _ RunBirthStore = (*FilesystemRunStore)(nil)

func (s *FilesystemRunStore) BeginRunBirth(ctx context.Context, id string) (RunBirthReservation, error) {
	if ctx == nil {
		return nil, ErrInvalidValueFormat
	}
	if !runBirthAtomicPublicationSupported() {
		return nil, ErrRunBirthUnavailable
	}
	if err := RequireRunValueFormat(ctx, id); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	format, err := ValueFormatForWrite(ctx, id)
	if err != nil {
		return nil, err
	}
	// This stable inode serializes new reservations. Historical LockRun may
	// still win the run directory before Publish; no-replace then refuses us.
	preparation, err := lockFile(filepath.Join(s.root, ".run-birth-locks", id), "prepare run "+id)
	if err != nil {
		return nil, err
	}
	b := &filesystemRunBirth{store: s, id: id, format: format, preparation: preparation, syncDir: syncRunBirthDir}
	fail := func(err error) (RunBirthReservation, error) { return nil, errors.Join(err, b.Unlock()) }
	root := filepath.Join(s.root, ".run-births")
	if err = os.MkdirAll(root, dirPerm); err != nil {
		return fail(err)
	}
	b.stage, err = os.MkdirTemp(root, "reservation-")
	if err != nil {
		return fail(err)
	}
	b.execution, err = lockRun(b.stage, "unpublished run "+id)
	if err != nil {
		return fail(err)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	return b, nil
}

func (b *filesystemRunBirth) Publish(ctx context.Context, spec RunCreateSpec) (*Run, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.released || b.used {
		return nil, ErrRunBirthState
	}
	b.used = true
	spec, err := spec.Normalize(ctx)
	if err != nil {
		return nil, err
	}
	if spec.ID != b.id || spec.Format != b.format {
		return nil, ErrValueFormatChanged
	}
	r, err := b.store.newRunFromSpec(ctx, spec)
	if err != nil {
		return nil, err
	}
	r.BirthID = NewRunBirthID()
	r.ExecutionEpoch = NewExecutionEpoch()
	r.CASVersion = 1
	data, err := marshalRunValueJSON(ctx, r)
	if err != nil {
		return nil, err
	}
	if err = WriteFileAtomicNew(filepath.Join(b.stage, "run.json"), data, filePerm); err != nil {
		return nil, err
	}
	if err = b.syncDir(b.stage); err != nil {
		return nil, err
	}
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	writer, err := b.store.acquireRunWriteLock(b.id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = writer.Unlock() }()
	if err = b.store.guardNotDeleted(b.id); err != nil {
		return nil, err
	}
	runs := filepath.Dir(b.store.runDir(b.id))
	if err = os.MkdirAll(runs, dirPerm); err != nil {
		return nil, err
	}
	if err = b.syncDir(b.store.root); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = RenameNoReplace(b.stage, b.store.runDir(b.id)); err != nil {
		return nil, err
	}
	// Rename commits visible ownership. Retain it before any fallible sync or
	// cancellation check; Unlock must never erase this destination.
	b.published = true
	err = errors.Join(b.syncDir(runs), b.syncDir(filepath.Dir(b.stage)), ctx.Err())
	if err != nil {
		return r, fmt.Errorf("store: owned run %s publication acknowledgement: %w", b.id, err)
	}
	return r, nil
}

func (b *filesystemRunBirth) Unlock() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.released {
		return b.releaseErr
	}
	b.released = true
	if b.execution != nil {
		b.releaseErr = errors.Join(b.releaseErr, b.execution.Unlock())
	}
	if !b.published && b.stage != "" {
		b.releaseErr = errors.Join(b.releaseErr, os.RemoveAll(b.stage))
	}
	if b.preparation != nil {
		b.releaseErr = errors.Join(b.releaseErr, b.preparation.Unlock())
	}
	return b.releaseErr
}

// syncRunBirthDir is deliberately strict: unlike legacy fsyncDir, unsupported
// directory synchronization cannot acknowledge durability of a new birth.
func syncRunBirthDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	return errors.Join(syncErr, closeErr)
}
