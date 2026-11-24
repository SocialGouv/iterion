package store

import (
	"context"
	"errors"
)

// OpenRunFilesGrant retains the original incarnation's directory. Historical
// runs keep the existing pathname API; this capability never silently falls back.
func (s *FilesystemRunStore) OpenRunFilesGrant(ctx context.Context, id string) (RunFilesGrant, error) {
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	_, unlock, err := s.lockRunMetadataMutation(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	format, _, err := s.valueRecordFormatLocked(ctx, id, false)
	if err != nil {
		return nil, err
	}
	return s.openBoundRunFilesGrantLocked(ctx, id, format)
}
func (s *FilesystemRunStore) openBoundRunFilesGrantLocked(ctx context.Context, id string, format ValueFormat) (RunFilesGrant, error) {
	binding, ok := RunDataBindingFrom(ctx)
	if !ok || binding.Header().Protocol != IncarnationRunData {
		return nil, ErrRunDataCapability
	}
	d, err := s.openRunPayloadDirectory(ctx, id, true, runFilesNamespacePart(format))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	f, err := d.parent.Open()
	if err != nil {
		return nil, err
	}
	modeErr := f.Chmod(0o775)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(modeErr, syncErr, closeErr, ctx.Err()); err != nil {
		return nil, err
	}
	root := d.parent.root
	g, err := NewRetainedRunFilesGrant(d.parent.key, root, binding, func(bound context.Context) error {
		_, unlock, err := s.lockExistingRunMetadata(bound, id)
		if err == nil {
			unlock()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	// Transfer the directory exactly once; the deferred temporary view closes no
	// descriptor after this point. The caller, not the store, owns the grant.
	d.parent.root = nil
	return g, nil
}
