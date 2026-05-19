package runview

import (
	"context"
	"io"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// WriteAttachment forwards to the underlying RunStore.
func (s *Service) WriteAttachment(ctx context.Context, runID string, rec store.AttachmentRecord, body io.Reader) error {
	return s.store.WriteAttachment(ctx, runID, rec, body)
}

// OpenAttachment forwards to the underlying RunStore.
func (s *Service) OpenAttachment(ctx context.Context, runID, name string) (io.ReadCloser, store.AttachmentRecord, error) {
	return s.store.OpenAttachment(ctx, runID, name)
}

// ListAttachments forwards to the underlying RunStore.
func (s *Service) ListAttachments(ctx context.Context, runID string) ([]store.AttachmentRecord, error) {
	return s.store.ListAttachments(ctx, runID)
}

// RemoveAttachment forwards to the underlying RunStore. Used by the
// HTTP layer's transactional rollback in promoteStaged.
func (s *Service) RemoveAttachment(ctx context.Context, runID, name string) error {
	return s.store.RemoveAttachment(ctx, runID, name)
}

// PresignAttachment forwards to the underlying RunStore.
func (s *Service) PresignAttachment(ctx context.Context, runID, name string, ttl time.Duration) (string, error) {
	return s.store.PresignAttachment(ctx, runID, name, ttl)
}

// VerifyAttachmentSignature checks an HMAC-signed presign URL when
// the underlying store implements signature verification (filesystem
// only — cloud stores rely on AWS SigV4). Returns false on cloud /
// non-FS stores.
func (s *Service) VerifyAttachmentSignature(runID, name, exp, sig string) bool {
	if s == nil || s.store == nil {
		return false
	}
	type verifier interface {
		VerifyAttachmentSignature(runID, name, exp, sig string) bool
	}
	if v, ok := s.store.(verifier); ok {
		return v.VerifyAttachmentSignature(runID, name, exp, sig)
	}
	return false
}
