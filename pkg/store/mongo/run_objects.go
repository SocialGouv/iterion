package mongo

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// captureRunObjects retains the original header and selects one immutable
// view before body reads, path grants, or remote I/O. It grants no authority.
func (s *Store) captureRunObjects(ctx context.Context, id string) (context.Context, blob.RunObjects, error) {
	// Tombstones keep their format header. Validate that header and the
	// caller's binding before readers can translate deletion into absence.
	parent, _, err := s.readRunFormatMetadata(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	var original store.RunValueFormat
	if parent == nil {
		// Preserve legacy orphan reads and explicit caller intent. An absent
		// parent is not evidence that an exact-format run was born.
		format, err := store.ValueFormatForWrite(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		original = store.RunValueFormat{RunID: id, Format: format}
	} else {
		if _, err := store.CheckRunFormatReplacement(ctx, parent, parent); err != nil {
			return nil, nil, err
		}
		if parent.DeletedAt != nil {
			return nil, nil, fmt.Errorf("store/mongo: run %s: %w", id, store.ErrRunDeleted)
		}
		original, err = store.CaptureRunValueFormat(parent)
		if err != nil {
			return nil, nil, err
		}
	}
	bound, err := store.WithRunValueFormat(ctx, original)
	if err != nil {
		return nil, nil, err
	}
	objects, err := s.selectRunObjects(bound, original.Format, original.RunID)
	return bound, objects, err
}

// runObjectsFromRun ties a reader's namespace to the same Run that supplied
// its metadata. Recapturing the current parent could mix two formats.
func (s *Store) runObjectsFromRun(ctx context.Context, r *store.Run) (context.Context, blob.RunObjects, error) {
	if _, err := store.CheckRunFormatReplacement(ctx, r, r); err != nil {
		return nil, nil, err
	}
	if err := store.CheckRunDataBinding(ctx, store.RunDataHeader{RunID: r.ID, Format: store.ValueFormat(r.FormatVersion), Protocol: r.DataProtocol, BirthID: r.BirthID}); err != nil {
		return nil, nil, err
	}
	original, err := store.CaptureRunValueFormat(r)
	if err != nil {
		return nil, nil, err
	}
	bound, err := store.WithRunValueFormat(ctx, original)
	if err != nil {
		return nil, nil, err
	}
	objects, err := s.selectRunObjects(bound, original.Format, original.RunID)
	return bound, objects, err
}

// checkRunObjects returns the boundary refusal after I/O without adopting a
// successor. Late bytes can remain orphaned in the old namespace; they cannot
// authorize metadata publication or affect a different format's readers.
func (s *Store) checkRunObjects(ctx context.Context, id string) error {
	_, err := s.prepareRunMetadataFormat(ctx, id)
	return err
}

// A deleted run's opaque bytes are absent to readers, preserving the original
// not-found causes. This conversion is never used by writers and does not
// soften unsupported headers, conflicting bindings, or infrastructure errors.
func runObjectsReadError(err error) error {
	if errors.Is(err, store.ErrRunDeleted) {
		return fmt.Errorf("store/mongo: deleted run has no readable objects: %w: %w", blob.ErrArtifactNotFound, os.ErrNotExist)
	}
	return err
}
