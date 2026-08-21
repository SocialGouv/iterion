package mongo

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

var _ store.BackendSessionStore = (*Store)(nil)

func (s *Store) PutBackendSession(ctx context.Context, runID, ref string, body []byte) error {
	if err := s.blob.PutBackendSession(ctx, runID, ref, body); err != nil {
		return fmt.Errorf("store/mongo: put backend session %s/%s: %w", runID, ref, err)
	}
	return nil
}

func (s *Store) GetBackendSession(ctx context.Context, runID, ref string) ([]byte, error) {
	b, err := s.blob.GetBackendSession(ctx, runID, ref)
	if err != nil {
		if errors.Is(err, blob.ErrArtifactNotFound) {
			return nil, fmt.Errorf("store/mongo: backend session %s/%s not found: %w", runID, ref, os.ErrNotExist)
		}
		return nil, fmt.Errorf("store/mongo: get backend session %s/%s: %w", runID, ref, err)
	}
	return b, nil
}

func (s *Store) DeleteBackendSession(ctx context.Context, runID, ref string) error {
	if err := s.blob.DeleteBackendSession(ctx, runID, ref); err != nil {
		return fmt.Errorf("store/mongo: delete backend session %s/%s: %w", runID, ref, err)
	}
	return nil
}
