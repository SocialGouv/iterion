package mongo

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// The cloud (Mongo) store satisfies the out-of-band IR blob seam, backed
// by the same S3 blob client that holds artifacts + tool blobs. It is the
// storage half of the queue's IRRef fallback: a workflow whose compiled IR
// exceeds the NATS max_payload is parked here by the publisher and fetched
// back by the runner. IR blobs can exceed the 16 MiB BSON ceiling, so they
// live in S3, not Mongo — matching artifacts + tool blobs.
var _ store.IRBlobStore = (*Store)(nil)

// PutIRBlob implements store.IRBlobStore: PUT the marshaled IR to S3 under
// ir/<runID>.json and return that key for queue.IRRef.StorageKey.
func (s *Store) PutIRBlob(ctx context.Context, runID string, body []byte) (string, error) {
	key, err := blob.IRBlobKey(runID)
	if err != nil {
		return "", err
	}
	if err := s.blob.PutIRBlob(ctx, runID, body); err != nil {
		return "", fmt.Errorf("store/mongo: put IR blob %s: %w", runID, err)
	}
	return key, nil
}

// GetIRBlob implements store.IRBlobStore: fetch the IR bytes addressed by
// the storage key carried on the queue message. A missing blob returns an
// os.ErrNotExist-compatible error.
func (s *Store) GetIRBlob(ctx context.Context, storageKey string) ([]byte, error) {
	body, err := s.blob.GetIRBlob(ctx, storageKey)
	if err != nil {
		if errors.Is(err, blob.ErrArtifactNotFound) {
			return nil, fmt.Errorf("store/mongo: IR blob %s not found: %w", storageKey, os.ErrNotExist)
		}
		return nil, fmt.Errorf("store/mongo: get IR blob %s: %w", storageKey, err)
	}
	return body, nil
}

// IRBlobBackend implements store.IRBlobStore: cloud IR blobs live in S3.
func (s *Store) IRBlobBackend() string { return "s3" }
