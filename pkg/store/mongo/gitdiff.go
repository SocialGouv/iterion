package mongo

import (
	"context"
	"fmt"
	"io"
)

// gitDiffBlobBucket is the attachment "name" segment under which offloaded
// per-file diff blobs live: attachments/<run_id>/__gitdiff/<ref>. It rides
// the existing attachment key layout so DeleteRunAttachments (invoked by
// DeleteRun) reclaims it in the same prefix sweep. It is deliberately NOT
// reflected into the runs.attachments index — these are internal diff
// snapshots, not operator-visible attachments.
const gitDiffBlobBucket = "__gitdiff"

// PutRunDiffBlob implements store.RunDiffBlobStore: PUT the diff body straight
// to the blob backend without touching the runs.attachments index. Idempotent
// (same ref → same key → replaced bytes).
func (s *Store) PutRunDiffBlob(ctx context.Context, runID, ref string, body []byte) error {
	if err := s.blob.PutAttachment(ctx, runID, gitDiffBlobBucket, ref, "application/json", body); err != nil {
		return fmt.Errorf("store/mongo: put diff blob %s/%s: %w", runID, ref, err)
	}
	return nil
}

// GetRunDiffBlob implements store.RunDiffBlobStore: stream the diff body back
// from the blob backend.
func (s *Store) GetRunDiffBlob(ctx context.Context, runID, ref string) ([]byte, error) {
	rc, _, err := s.blob.GetAttachment(ctx, runID, gitDiffBlobBucket, ref)
	if err != nil {
		return nil, fmt.Errorf("store/mongo: get diff blob %s/%s: %w", runID, ref, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("store/mongo: read diff blob %s/%s: %w", runID, ref, err)
	}
	return body, nil
}
