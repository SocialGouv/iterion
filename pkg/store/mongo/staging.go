package mongo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// The cloud twin of the filesystem store's <root>/uploads/: the record of a
// staged upload lives in the staged_uploads collection, tenant-stamped, and
// its bytes in the blob backend under uploads/<upload_id>/<filename>. The
// server pod has no disk a launch could read back from; the blob is the
// one place every replica reaches. Nothing presigns an upload: the record,
// read under the caller's tenant, is the only way to its bytes.
var _ store.StagingStore = (*Store)(nil)

// StageUpload implements store.StagingStore. The record goes first so the
// sweep can find the bytes whatever happens next; a PUT that fails takes
// the record back out.
func (s *Store) StageUpload(ctx context.Context, rec store.StagedUpload, body io.ReadSeeker) error {
	if _, err := blob.UploadKey(rec.UploadID, rec.OriginalFilename); err != nil {
		return fmt.Errorf("store/mongo: stage upload: %w", err)
	}
	if rec.Size <= 0 {
		return fmt.Errorf("store/mongo: stage upload %s: the record carries no size", rec.UploadID)
	}
	if id, ok := store.TenantFromContext(ctx); ok {
		rec.TenantID = id
	}
	if uid, ok := store.OwnerFromContext(ctx); ok {
		rec.OwnerID = uid
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	if _, err := s.stagedUploads.InsertOne(ctx, rec); err != nil {
		return fmt.Errorf("store/mongo: stage upload %s: record: %w", rec.UploadID, err)
	}
	if err := s.blob.PutUpload(ctx, rec.UploadID, rec.OriginalFilename, rec.MIME, body, rec.Size); err != nil {
		// A PUT that failed may still have stored the object (a connection
		// dropped after the write): the bytes go with the record, or the
		// record stays for the sweep — never bytes with no record.
		if rmErr := s.removeStagedUpload(ctx, rec.UploadID); rmErr != nil && s.logger != nil {
			s.logger.Warn("store/mongo: stage upload %s: the failed put left a record for the sweep: %v", rec.UploadID, rmErr)
		}
		return fmt.Errorf("store/mongo: stage upload %s: blob put: %w", rec.UploadID, err)
	}
	return nil
}

// OpenStagedUpload implements store.StagingStore: the record is read under
// the caller's tenant, so another tenant's upload is not found.
func (s *Store) OpenStagedUpload(ctx context.Context, uploadID string) (store.StagedUpload, io.ReadCloser, error) {
	var rec store.StagedUpload
	err := s.stagedUploads.FindOne(ctx, withTenantFilter(ctx, bson.M{"_id": uploadID})).Decode(&rec)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return store.StagedUpload{}, nil, store.ErrUploadNotFound
		}
		return store.StagedUpload{}, nil, fmt.Errorf("store/mongo: open staged upload %s: %w", uploadID, err)
	}
	rc, _, err := s.blob.GetUpload(ctx, rec.UploadID, rec.OriginalFilename)
	if err != nil {
		if errors.Is(err, blob.ErrArtifactNotFound) {
			return store.StagedUpload{}, nil, store.ErrUploadNotFound
		}
		return store.StagedUpload{}, nil, fmt.Errorf("store/mongo: open staged upload %s: blob get: %w", uploadID, err)
	}
	return rec, rc, nil
}

// DiscardStagedUpload implements store.StagingStore. Ownership is settled
// by the tenant-filtered read; then the bytes go before the record — a
// record left behind is swept later, bytes left behind never would be.
func (s *Store) DiscardStagedUpload(ctx context.Context, uploadID string) error {
	var rec store.StagedUpload
	err := s.stagedUploads.FindOne(ctx, withTenantFilter(ctx, bson.M{"_id": uploadID})).Decode(&rec)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil
		}
		return fmt.Errorf("store/mongo: discard staged upload %s: %w", uploadID, err)
	}
	return s.removeStagedUpload(ctx, rec.UploadID)
}

func (s *Store) removeStagedUpload(ctx context.Context, uploadID string) error {
	if err := s.blob.DeleteUpload(ctx, uploadID); err != nil {
		return fmt.Errorf("store/mongo: discard staged upload %s: blob delete: %w", uploadID, err)
	}
	if _, err := s.stagedUploads.DeleteOne(ctx, bson.M{"_id": uploadID}); err != nil {
		return fmt.Errorf("store/mongo: discard staged upload %s: record: %w", uploadID, err)
	}
	return nil
}

// ReapStagedUploads implements store.StagingStore. A platform sweep: it
// reads the collection without the tenant guard, because no tenant owns
// the question "what is older than an hour"; it runs on every replica, and
// two sweeps of the same upload both end in nothing to remove.
func (s *Store) ReapStagedUploads(ctx context.Context, cutoff time.Time) (int, error) {
	cur, err := s.stagedUploads.Find(ctx, bson.M{"created_at": bson.M{"$lt": cutoff}})
	if err != nil {
		return 0, fmt.Errorf("store/mongo: reap staged uploads: %w", err)
	}
	var due []store.StagedUpload
	if err := cur.All(ctx, &due); err != nil {
		return 0, fmt.Errorf("store/mongo: reap staged uploads: %w", err)
	}
	reaped := 0
	var errs []error
	for _, rec := range due {
		if err := s.removeStagedUpload(ctx, rec.UploadID); err != nil {
			errs = append(errs, err)
			continue
		}
		reaped++
	}
	if len(errs) > 0 {
		return reaped, errors.Join(errs...)
	}
	return reaped, nil
}
