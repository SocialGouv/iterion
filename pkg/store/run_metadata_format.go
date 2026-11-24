package store

import "context"

// lockRunMetadataMutation acquires s.mu then the stable run lock. Its caller
// must defer unlock immediately after checking err and use no-relock tails.
// A nil parent preserves the legacy orphan-sidecar case.
func (s *FilesystemRunStore) lockRunMetadataMutation(ctx context.Context, id string) (*Run, func(), error) {
	if ctx == nil {
		return nil, nil, ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	lock, err := s.acquireRunWriteLock(id)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	unlock := func() { _ = lock.Unlock(); s.mu.Unlock() }
	_, parent, err := s.valueRecordFormatLocked(ctx, id, false)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return parent, unlock, nil
}

// lockExistingRunMetadata is the noncreating Run mutation entry point.
func (s *FilesystemRunStore) lockExistingRunMetadata(ctx context.Context, id string) (*Run, func(), error) {
	parent, unlock, err := s.lockRunMetadataMutation(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if parent == nil {
		unlock()
		return nil, nil, ErrRunNotFound
	}
	return parent, unlock, nil
}

// withRunMetadataMutation owns the complete metadata/sidecar mutation. The
// callback must use private no-relock helpers; it never receives a grant.
// Unbound metadata accepts known exact runs without changing payload defaults.
func (s *FilesystemRunStore) withRunMetadataMutation(ctx context.Context, id string, apply func(*Run) error) error {
	parent, unlock, err := s.lockRunMetadataMutation(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return apply(parent)
}
