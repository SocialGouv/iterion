package mongo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Attachments are persisted in two places in cloud mode:
//   - bytes go to the blob backend at attachments/<run_id>/<name>/<filename>
//   - metadata is reflected on the runs collection under r.attachments
//
// Cross-pod readers (the runner pod) GET from the blob backend; the
// runs collection is the single source of truth for "which attachments
// exist for this run", which is required for resume to be deterministic.

func (s *Store) WriteAttachment(ctx context.Context, runID string, rec store.AttachmentRecord, body io.Reader) error {
	if rec.Name == "" {
		return errors.New("store/mongo: attachment name required")
	}
	if err := s.guardNotDeleted(ctx, runID); err != nil {
		return err
	}
	filename := rec.OriginalFilename
	if filename == "" {
		filename = rec.Name
	}
	// Stream-read into memory while hashing, bounded by the configured
	// cap so an unbounded body can't push the runner pod into OOM.
	// LimitReader+1 lets us detect overflow: if we got past `cap` bytes,
	// the body was over the limit and we reject it.
	maxBytes := s.maxAttachmentBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxAttachmentBytes
	}
	hasher := sha256.New()
	limited := io.LimitReader(body, maxBytes+1)
	buf, err := io.ReadAll(io.TeeReader(limited, hasher))
	if err != nil {
		return fmt.Errorf("store/mongo: read attachment body: %w", err)
	}
	if int64(len(buf)) > maxBytes {
		return fmt.Errorf("store/mongo: attachment %q exceeds %d-byte limit", rec.Name, maxBytes)
	}
	if rec.MIME == "" {
		rec.MIME = "application/octet-stream"
	}
	rec.OriginalFilename = filename
	rec.Size = int64(len(buf))
	rec.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	ref, err := blob.AttachmentKey(runID, rec.Name, rec.OriginalFilename)
	if err != nil {
		return fmt.Errorf("store/mongo: attachment key: %w", err)
	}
	rec.StorageRef = ref

	if err := s.blob.PutAttachment(ctx, runID, rec.Name, rec.OriginalFilename, rec.MIME, buf); err != nil {
		return fmt.Errorf("store/mongo: blob put attachment: %w", err)
	}

	// Reflect into runs collection. Use $set on the nested key so a
	// concurrent attachment write to a different name doesn't lose
	// the document-level race.
	_, err = s.runs.UpdateOne(ctx,
		withTenantFilter(ctx, bson.M{"_id": runID}),
		bson.M{
			"$set": bson.M{
				"attachments." + rec.Name: rec,
				"updated_at":              time.Now().UTC(),
			},
		},
	)
	if err != nil {
		return fmt.Errorf("store/mongo: index attachment in run: %w", err)
	}
	return nil
}

func (s *Store) OpenAttachment(ctx context.Context, runID, name string) (io.ReadCloser, store.AttachmentRecord, error) {
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		return nil, store.AttachmentRecord{}, err
	}
	rec, ok := r.Attachments[name]
	if !ok {
		return nil, store.AttachmentRecord{}, store.ErrAttachmentNotFound
	}
	rc, _, err := s.blob.GetAttachment(ctx, runID, name, rec.OriginalFilename)
	if err != nil {
		return nil, rec, fmt.Errorf("store/mongo: blob get attachment: %w", err)
	}
	return rc, rec, nil
}

func (s *Store) ListAttachments(ctx context.Context, runID string) ([]store.AttachmentRecord, error) {
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]store.AttachmentRecord, 0, len(r.Attachments))
	for _, rec := range r.Attachments {
		out = append(out, rec)
	}
	return out, nil
}

func (s *Store) RemoveAttachment(ctx context.Context, runID, name string) error {
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		return err
	}
	rec, ok := r.Attachments[name]
	if !ok {
		return nil
	}
	if err := s.blob.DeleteAttachment(ctx, runID, name, rec.OriginalFilename); err != nil {
		return fmt.Errorf("store/mongo: blob delete attachment %s/%s: %w", runID, name, err)
	}
	_, err = s.runs.UpdateOne(ctx,
		withTenantFilter(ctx, bson.M{"_id": runID}),
		bson.M{
			"$unset": bson.M{"attachments." + name: ""},
			"$set":   bson.M{"updated_at": time.Now().UTC()},
		},
	)
	if err != nil {
		return fmt.Errorf("store/mongo: clear attachment %s/%s: %w", runID, name, err)
	}
	return nil
}

func (s *Store) DeleteRunAttachments(ctx context.Context, runID string) error {
	if err := s.blob.DeleteRunAttachments(ctx, runID); err != nil {
		return fmt.Errorf("store/mongo: blob delete attachments: %w", err)
	}
	_, err := s.runs.UpdateOne(ctx,
		withTenantFilter(ctx, bson.M{"_id": runID}),
		bson.M{"$unset": bson.M{"attachments": ""}, "$set": bson.M{"updated_at": time.Now().UTC()}},
	)
	if err != nil {
		return fmt.Errorf("store/mongo: clear attachments index: %w", err)
	}
	return nil
}

func (s *Store) PresignAttachment(ctx context.Context, runID, name string, ttl time.Duration) (string, error) {
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		return "", err
	}
	rec, ok := r.Attachments[name]
	if !ok {
		return "", store.ErrAttachmentNotFound
	}
	return s.blob.PresignAttachment(ctx, runID, name, rec.OriginalFilename, ttl)
}
