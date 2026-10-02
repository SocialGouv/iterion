package mongo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

var _ store.ScratchBankStore = (*Store)(nil)

// scratchBanker is the blob client's streaming surface, or an error naming
// a client that has none.
func (s *Store) scratchBanker() (blob.ScratchBanker, error) {
	b, ok := s.blob.(blob.ScratchBanker)
	if !ok {
		return nil, fmt.Errorf("store/mongo: the blob client (%T) keeps no scratch bank", s.blob)
	}
	return b, nil
}

func (s *Store) PutScratchBank(ctx context.Context, runID string, body io.Reader, size int64) error {
	b, err := s.scratchBanker()
	if err != nil {
		return err
	}
	if err := b.PutScratchBank(ctx, runID, body, size); err != nil {
		return fmt.Errorf("store/mongo: put scratch bank %s: %w", runID, err)
	}
	return nil
}

func (s *Store) OpenScratchBank(ctx context.Context, runID string) (io.ReadCloser, error) {
	b, err := s.scratchBanker()
	if err != nil {
		return nil, err
	}
	rc, err := b.OpenScratchBank(ctx, runID)
	if err != nil {
		if errors.Is(err, blob.ErrArtifactNotFound) {
			return nil, fmt.Errorf("store/mongo: scratch bank %s not found: %w", runID, os.ErrNotExist)
		}
		return nil, fmt.Errorf("store/mongo: open scratch bank %s: %w", runID, err)
	}
	return rc, nil
}

func (s *Store) DeleteScratchBank(ctx context.Context, runID string) error {
	b, err := s.scratchBanker()
	if err != nil {
		return err
	}
	if err := b.DeleteScratchBank(ctx, runID); err != nil {
		return fmt.Errorf("store/mongo: delete scratch bank %s: %w", runID, err)
	}
	return nil
}
