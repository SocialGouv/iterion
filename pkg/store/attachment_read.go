package store

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrAttachmentSignature identifies an invalid or expired local attachment URL.
var ErrAttachmentSignature = errors.New("invalid attachment signature")

// PresignedAttachmentReader opens a locally signed attachment under the URL's
// original value-format binding. Cloud presigned URLs are served by the blob
// backend and do not implement this local HTTP capability.
type PresignedAttachmentReader interface {
	OpenPresignedAttachment(ctx context.Context, runID, name, exp, sig, valueFormat string) (io.ReadCloser, AttachmentRecord, error)
}

func AsPresignedAttachmentReader(s RunStore) PresignedAttachmentReader {
	return capability[PresignedAttachmentReader](s)
}

// lockAttachmentRead admits under mu -> stable lock. Historical filesystem
// tombstones contain no header, so check context and RunID before preserving
// attachment-not-found compatibility. No inter-format comparison is invented
// after deletion. Missing parents retain the legacy orphan reader contract.
func (s *FilesystemRunStore) lockAttachmentRead(ctx context.Context, id string) (ValueFormat, func(), error) {
	original, err := ValueFormatForWrite(ctx, id)
	if err != nil {
		return 0, nil, err
	}
	// Keep deletion classification under the same exclusion as the header and
	// open. The generic metadata mutation helper refuses deleted runs before
	// reading their possible remaining header, which a read must not hide.
	s.mu.Lock()
	lock, err := s.acquireRunWriteLock(id)
	if err != nil {
		s.mu.Unlock()
		return 0, nil, err
	}
	unlock := func() { _ = lock.Unlock(); s.mu.Unlock() }
	format, _, err := s.valueRecordFormatLocked(ctx, id, false)
	if errors.Is(err, ErrRunDeleted) {
		// A failed cleanup can leave a header beside its tombstone. Validate that
		// real header; only a genuinely headerless tombstone lacks a format.
		parent, readErr := s.loadRunRawContext(ctx, id)
		if readErr != nil && !errors.Is(readErr, ErrRunDeleted) && !errors.Is(readErr, ErrRunNotFound) {
			unlock()
			return 0, nil, readErr
		}
		format = original
		if parent != nil {
			format, err = NormalizeValueFormat(parent.FormatVersion)
			if err == nil {
				if _, bound := ctx.Value(runValueFormatKey{}).(RunValueFormat); bound {
					format, err = CheckRunValueFormat(ctx, id, parent.FormatVersion)
				}
			}
			if err != nil {
				unlock()
				return 0, nil, err
			}
		}
		if err := ctx.Err(); err != nil {
			unlock()
			return 0, nil, err
		}
		unlock()
		return format, nil, ErrAttachmentNotFound
	}
	if err != nil {
		unlock()
		return 0, nil, err
	}
	return format, unlock, nil
}

// OpenPresignedAttachment verifies the signed identity before opening bytes.
// URLs without vf use the historical MAC and retain legacy intent forever.
// New exact URLs authenticate vf=2 as part of a distinct MAC plaintext. Caller
// bindings are cumulative: a signed URL cannot replace a conflicting binding.
func (s *FilesystemRunStore) OpenPresignedAttachment(ctx context.Context, runID, name, exp, sig, valueFormat string) (io.ReadCloser, AttachmentRecord, error) {
	if _, err := ValueFormatForWrite(ctx, runID); err != nil {
		return nil, AttachmentRecord{}, err
	}
	format := LegacyValueFormat
	switch valueFormat {
	case "":
	case "2":
		format = ExactValueFormat
	default:
		return nil, AttachmentRecord{}, ErrAttachmentSignature
	}
	if !s.verifyAttachmentSignature(runID, name, exp, sig, format) {
		return nil, AttachmentRecord{}, ErrAttachmentSignature
	}
	if _, ok := ctx.Value(runValueFormatKey{}).(RunValueFormat); ok {
		original, err := ValueFormatForWrite(ctx, runID)
		if err != nil {
			return nil, AttachmentRecord{}, err
		}
		if original != format {
			return nil, AttachmentRecord{}, fmt.Errorf("%w: signed attachment differs from original binding", ErrValueFormatChanged)
		}
	}
	bound, err := WithRunValueFormat(ctx, RunValueFormat{RunID: runID, Format: format})
	if err != nil {
		return nil, AttachmentRecord{}, err
	}
	return s.OpenAttachment(bound, runID, name)
}
