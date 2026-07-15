package mongo

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// The cloud (Mongo) store satisfies the per-tool-call I/O blob seam,
// backed by the same S3 blob client that holds artifacts + attachments.
// Before this twin, AsToolBlobStore returned nil for cloud runs, so
// GET /api/runs/{id}/tools/{toolUseID}/{kind} 503'd and expanding a
// large tool output in the studio's Tools tab failed. Tool bodies can
// exceed the 16 MiB BSON document ceiling, so they live in S3, not Mongo
// — the events collection still carries the small inline preview + ref.
var _ store.ToolBlobStore = (*Store)(nil)

// validateToolBlobKind rejects anything but the two known literals so a
// network-sourced kind can never widen the S3 key space.
func validateToolBlobKind(kind string) error {
	if kind != "input" && kind != "output" {
		return fmt.Errorf("store/mongo: tool blob kind must be input|output, got %q", kind)
	}
	return nil
}

// WriteToolBlob implements store.ToolBlobStore: PUT the body to S3 under
// tools/<runID>/<toolUseID>/<kind>. Idempotent — re-writing the same key
// replaces the prior bytes. Returns the byte size written.
func (s *Store) WriteToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) (int64, error) {
	if err := validateToolBlobKind(kind); err != nil {
		return 0, err
	}
	if err := s.blob.PutToolBlob(ctx, runID, toolUseID, kind, body); err != nil {
		return 0, fmt.Errorf("store/mongo: write tool blob %s/%s/%s: %w", runID, toolUseID, kind, err)
	}
	return int64(len(body)), nil
}

// ReadToolBlob implements store.ToolBlobStore: read up to `limit` bytes
// from `offset` (limit==0 → all from offset). Returns the bytes, the full
// size, and eof. A missing blob returns an os.ErrNotExist-compatible
// error so the HTTP surface maps it to a 404 (mirroring the filesystem
// store, whose os.Open error is already os.IsNotExist-compatible).
//
// Tenant isolation is enforced one layer up: the HTTP handler loads the
// run under the caller's tenant ctx before reaching here (cross-tenant →
// 404), exactly as OpenAttachment gates the attachment blob. S3 keys are
// not tenant-prefixed, matching artifacts + attachments.
func (s *Store) ReadToolBlob(ctx context.Context, runID, toolUseID, kind string, offset, limit int64) ([]byte, int64, bool, error) {
	if err := validateToolBlobKind(kind); err != nil {
		return nil, 0, false, err
	}
	data, total, eof, err := s.blob.GetToolBlobRange(ctx, runID, toolUseID, kind, offset, limit)
	if err != nil {
		if errors.Is(err, blob.ErrArtifactNotFound) {
			return nil, 0, true, fmt.Errorf("store/mongo: tool blob %s/%s/%s not found: %w", runID, toolUseID, kind, os.ErrNotExist)
		}
		return nil, 0, false, fmt.Errorf("store/mongo: read tool blob %s/%s/%s: %w", runID, toolUseID, kind, err)
	}
	return data, total, eof, nil
}
