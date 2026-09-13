package mongo

import (
	"context"
	"errors"
	"io"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

func (s *Store) portFileStagingDir(id string) string {
	return filepath.Join(s.runFilesScratch, "ports-v1-publications", id)
}

func (s *Store) PutPortFile(ctx context.Context, ref store.PortFileRef, content io.Reader) error {
	if err := s.guardNativeRun(ctx, ref.RunID); err != nil {
		return err
	}
	snapshot, err := store.PreparePortFile(ctx, s.portFileStagingDir(ref.RunID), ref, content)
	if err != nil {
		return err
	}
	defer func() { _ = snapshot.Close() }()
	body, _, err := s.blob.GetRunFile(ctx, ref.RunID, ref.Path)
	if err == nil {
		return errors.Join(store.VerifyPortFile(ctx, ref, body), body.Close())
	}
	if !errors.Is(err, blob.ErrArtifactNotFound) {
		return err
	}
	// Only a verified private snapshot reaches PUT. Racing producers of this
	// content-addressed key can therefore write only the same bytes. No
	// already-published key is overwritten when a producer supplies bad data.
	return s.blob.PutRunFile(ctx, ref.RunID, ref.Path, ref.MediaType, snapshot, ref.Size)
}
