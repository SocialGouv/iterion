package blob

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"io"
	"strings"
	"time"
)

// RunObjects is one immutable, format-bound view of run-owned bytes. It
// deliberately has no artifacts, IR, uploads or client-lifecycle operations.
// ScratchBanker remains optional; a view must not invent this capability.
type RunObjects interface {
	PutAttachment(ctx context.Context, runID, name, filename, contentType string, body []byte) error
	GetAttachment(ctx context.Context, runID, name, filename string) (io.ReadCloser, AttachmentMeta, error)
	PresignAttachment(ctx context.Context, runID, name, filename string, ttl time.Duration) (string, error)
	DeleteAttachment(ctx context.Context, runID, name, filename string) error
	DeleteRunAttachments(ctx context.Context, runID string) error
	PutToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) error
	GetToolBlobRange(ctx context.Context, runID, toolUseID, kind string, offset, limit int64) (data []byte, total int64, eof bool, err error)
	DeleteRunToolBlobs(ctx context.Context, runID string) error
	PutBackendSession(ctx context.Context, runID, ref string, body []byte) error
	GetBackendSession(ctx context.Context, runID, ref string) ([]byte, error)
	DeleteBackendSession(ctx context.Context, runID, ref string) error
	DeleteRunBackendSessions(ctx context.Context, runID string) error
	PutRunFile(ctx context.Context, runID, relPath, contentType string, body io.Reader, size int64) error
	ListRunFiles(ctx context.Context, runID string) ([]RunFileObject, error)
	GetRunFile(ctx context.Context, runID, relPath string) (io.ReadCloser, RunFileObject, error)
	DeleteRunFiles(ctx context.Context, runID string) error
}

// ValueFormatRunObjects selects a closed namespace once, before any I/O.
// Implementations must copy the format by value and never rebind on a retry.
// This is format isolation, not an incarnation fence or a revocable handle.
type ValueFormatRunObjects interface {
	RunObjectsForFormat(int) (RunObjects, error)
}

// SelectRunObjects preserves clients without the new capability only for
// legacy bytes. Exact-format callers must never fall back to legacy keys.
func SelectRunObjects(client Client, raw int) (RunObjects, error) {
	format, err := store.NormalizeValueFormat(raw)
	if err != nil {
		return nil, err
	}
	if capable, ok := client.(ValueFormatRunObjects); ok {
		return capable.RunObjectsForFormat(int(format))
	}
	if format != store.LegacyValueFormat {
		return nil, fmt.Errorf("%w: blob RunObjects", store.ErrValueFormatCapability)
	}
	narrow := legacyRunObjects{RunObjects: client}
	if scratch, ok := client.(ScratchBanker); ok {
		return legacyScratchRunObjects{legacyRunObjects: narrow, ScratchBanker: scratch}, nil
	}
	return narrow, nil
}

type legacyRunObjects struct{ RunObjects }
type legacyScratchRunObjects struct {
	legacyRunObjects
	ScratchBanker
}

// AttachmentKeyForFormat is the canonical StorageRef for an attachment.
func AttachmentKeyForFormat(raw int, runID, name, filename string) (string, error) {
	f, err := store.NormalizeValueFormat(raw)
	if err != nil {
		return "", err
	}
	key, err := attachmentKey(runID, name, filename)
	if err != nil {
		return "", err
	}
	return runObjectKeyForFormat(f, key), nil
}

// The roots are closed, not caller-configurable prefixes. Historical roots
// are byte-identical and their sweeps cannot touch any exact-format root.
func runObjectKeyForFormat(f store.ValueFormat, key string) string {
	if f != store.ExactValueFormat {
		return key
	}
	root, tail, _ := strings.Cut(key, "/")
	switch root {
	case "attachments", "tools", "sessions", "runfiles":
		return root + "-v2/" + tail
	default:
		panic("blob: internal non-run-object namespace")
	}
}

// A named field rather than an embedded client prevents method promotion:
// the view cannot Close the shared client or expose unrelated namespaces.
type s3RunObjects struct{ client S3Client }

func (c *S3Client) RunObjectsForFormat(raw int) (RunObjects, error) {
	f, err := store.NormalizeValueFormat(raw)
	if err != nil {
		return nil, err
	}
	copy := *c
	copy.runObjectsFormat = f
	return &s3RunObjects{client: copy}, nil
}

func (v *s3RunObjects) PutAttachment(ctx context.Context, runID, name, filename, contentType string, body []byte) error {
	return v.client.PutAttachment(ctx, runID, name, filename, contentType, body)
}
func (v *s3RunObjects) GetAttachment(ctx context.Context, runID, name, filename string) (io.ReadCloser, AttachmentMeta, error) {
	return v.client.GetAttachment(ctx, runID, name, filename)
}
func (v *s3RunObjects) PresignAttachment(ctx context.Context, runID, name, filename string, ttl time.Duration) (string, error) {
	return v.client.PresignAttachment(ctx, runID, name, filename, ttl)
}
func (v *s3RunObjects) DeleteAttachment(ctx context.Context, runID, name, filename string) error {
	return v.client.DeleteAttachment(ctx, runID, name, filename)
}
func (v *s3RunObjects) DeleteRunAttachments(ctx context.Context, runID string) error {
	return v.client.DeleteRunAttachments(ctx, runID)
}
func (v *s3RunObjects) PutToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) error {
	return v.client.PutToolBlob(ctx, runID, toolUseID, kind, body)
}
func (v *s3RunObjects) GetToolBlobRange(ctx context.Context, runID, toolUseID, kind string, offset, limit int64) ([]byte, int64, bool, error) {
	return v.client.GetToolBlobRange(ctx, runID, toolUseID, kind, offset, limit)
}
func (v *s3RunObjects) DeleteRunToolBlobs(ctx context.Context, runID string) error {
	return v.client.DeleteRunToolBlobs(ctx, runID)
}
func (v *s3RunObjects) PutBackendSession(ctx context.Context, runID, ref string, body []byte) error {
	return v.client.PutBackendSession(ctx, runID, ref, body)
}
func (v *s3RunObjects) GetBackendSession(ctx context.Context, runID, ref string) ([]byte, error) {
	return v.client.GetBackendSession(ctx, runID, ref)
}
func (v *s3RunObjects) DeleteBackendSession(ctx context.Context, runID, ref string) error {
	return v.client.DeleteBackendSession(ctx, runID, ref)
}
func (v *s3RunObjects) DeleteRunBackendSessions(ctx context.Context, runID string) error {
	return v.client.DeleteRunBackendSessions(ctx, runID)
}
func (v *s3RunObjects) PutRunFile(ctx context.Context, runID, relPath, contentType string, body io.Reader, size int64) error {
	return v.client.PutRunFile(ctx, runID, relPath, contentType, body, size)
}
func (v *s3RunObjects) ListRunFiles(ctx context.Context, runID string) ([]RunFileObject, error) {
	return v.client.ListRunFiles(ctx, runID)
}
func (v *s3RunObjects) GetRunFile(ctx context.Context, runID, relPath string) (io.ReadCloser, RunFileObject, error) {
	return v.client.GetRunFile(ctx, runID, relPath)
}
func (v *s3RunObjects) DeleteRunFiles(ctx context.Context, runID string) error {
	return v.client.DeleteRunFiles(ctx, runID)
}
func (v *s3RunObjects) PutScratchBank(ctx context.Context, runID string, body io.Reader, size int64) error {
	return v.client.PutScratchBank(ctx, runID, body, size)
}
func (v *s3RunObjects) OpenScratchBank(ctx context.Context, runID string) (io.ReadCloser, error) {
	return v.client.OpenScratchBank(ctx, runID)
}
func (v *s3RunObjects) DeleteScratchBank(ctx context.Context, runID string) error {
	return v.client.DeleteScratchBank(ctx, runID)
}
func (c *S3Client) attachmentKey(runID, name, filename string) (string, error) {
	key, err := attachmentKey(runID, name, filename)
	if err != nil {
		return "", err
	}
	return c.runObjectKey(runID, key)
}
func (c *S3Client) attachmentRunPrefix(runID string) (string, error) {
	key, err := attachmentRunPrefix(runID)
	if err != nil {
		return "", err
	}
	return c.runObjectKey(runID, key)
}
func (c *S3Client) toolBlobKey(runID, toolUseID, kind string) (string, error) {
	key, err := toolBlobKey(runID, toolUseID, kind)
	if err != nil {
		return "", err
	}
	return c.runObjectKey(runID, key)
}
func (c *S3Client) toolBlobRunPrefix(runID string) (string, error) {
	key, err := toolBlobRunPrefix(runID)
	if err != nil {
		return "", err
	}
	return c.runObjectKey(runID, key)
}
func (c *S3Client) backendSessionKey(runID, ref string) (string, error) {
	key, err := backendSessionKey(runID, ref)
	if err != nil {
		return "", err
	}
	return c.runObjectKey(runID, key)
}
func (c *S3Client) backendSessionRunPrefix(runID string) (string, error) {
	key, err := backendSessionRunPrefix(runID)
	if err != nil {
		return "", err
	}
	return c.runObjectKey(runID, key)
}
func (c *S3Client) runFileKey(runID, relPath string) (string, error) {
	key, err := runFileKey(runID, relPath)
	if err != nil {
		return "", err
	}
	return c.runObjectKey(runID, key)
}
func (c *S3Client) runFileRunPrefix(runID string) (string, error) {
	key, err := runFileRunPrefix(runID)
	if err != nil {
		return "", err
	}
	return c.runObjectKey(runID, key)
}

var _ RunObjects = (*s3RunObjects)(nil)
var _ ScratchBanker = (*s3RunObjects)(nil)
var _ ValueFormatRunObjects = (*S3Client)(nil)
