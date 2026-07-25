// Package blob defines the artifact-blob interface implemented by S3
// (cloud) and (potentially) a local filesystem variant for testing.
//
// The S3 implementation lands in plan §F T-16. This file is the
// minimum surface area the Mongo store needs to compile against
// without forcing the AWS SDK as a dependency on every CLI build.
package blob

import (
	"context"
	"io"
	"time"
)

// Client is the abstraction over a blob backend. Operations are
// idempotent: PutArtifact PUTs a deterministic key, so two writes of
// the same (run_id, node_id, version) produce byte-identical objects.
//
// This is the contract the Mongo store will consume in plan §F T-18:
// WriteArtifact PUTs through the blob, then inserts the
// `artifact_written` event in Mongo. The blob is the source of truth
// for body contents; the event is the source of truth for "this
// version exists".
type Client interface {
	// PutArtifact uploads body under
	// `artifacts/<runID>/<nodeID>/<version>.json`. Idempotent.
	PutArtifact(ctx context.Context, runID, nodeID string, version int, body []byte) error

	// GetArtifact returns the body previously PUT under the same key.
	// Returns an error wrapping a "not found" sentinel when the
	// version doesn't exist (impl-defined sentinel — callers should
	// use errors.Is against a backend-specific error or treat any
	// error as missing).
	GetArtifact(ctx context.Context, runID, nodeID string, version int) ([]byte, error)

	// ListArtifactVersions returns the set of versions persisted for
	// (runID, nodeID), in arbitrary order. Cloud impl can derive this
	// from a LIST prefix; the canonical ordering is "by event seq",
	// computed at the call site.
	ListArtifactVersions(ctx context.Context, runID, nodeID string) ([]int, error)

	// DeleteRun removes every blob under `artifacts/<runID>/` in a
	// single sweep. Used by retention sweepers and the migration tool
	// (plan §F T-42). Best-effort: partial failures must be logged
	// but should not break the sweeper.
	DeleteRun(ctx context.Context, runID string) error

	// Ping verifies the backend is reachable and the configured bucket
	// exists. Used by the server's /readyz handler. Cheap (HEAD) but
	// not free — callers should wrap in a sub-second timeout.
	Ping(ctx context.Context) error

	// Close releases any pooled HTTP connections / SDK resources
	// associated with the client. Safe to call multiple times.
	// Boot paths that fail partway through a multi-component init
	// rely on this to avoid leaking idle file descriptors.
	Close() error

	// PutAttachment uploads `body` (already buffered in memory) under
	// `attachments/<runID>/<name>/<filename>` with the given Content-Type.
	// Idempotent: a PUT with the same key replaces the bytes.
	PutAttachment(ctx context.Context, runID, name, filename, contentType string, body []byte) error

	// GetAttachment streams the bytes previously PUT under the same
	// key. Callers must Close the returned reader. AttachmentMeta
	// carries the Content-Type and Size as observed by the backend.
	GetAttachment(ctx context.Context, runID, name, filename string) (io.ReadCloser, AttachmentMeta, error)

	// PresignAttachment returns a time-limited URL the caller can
	// hand to a third party (browser, agent fetch). ttl bounds the
	// validity of the URL; backends typically clamp to a maximum
	// (e.g. 7 days for SigV4).
	PresignAttachment(ctx context.Context, runID, name, filename string, ttl time.Duration) (string, error)

	// DeleteAttachment removes the blob for a single attachment
	// (`attachments/<runID>/<name>/<filename>`). Used by transactional
	// rollback paths where only one of several uploads must be undone.
	// Idempotent: deleting a non-existent key returns nil.
	DeleteAttachment(ctx context.Context, runID, name, filename string) error

	// DeleteRunAttachments removes every blob under
	// `attachments/<runID>/` in a single sweep. Best-effort: partial
	// failures must be logged but should not break sweepers.
	DeleteRunAttachments(ctx context.Context, runID string) error

	// PutToolBlob uploads a per-tool-call I/O body under
	// `tools/<runID>/<toolUseID>/<kind>` (kind ∈ {input,output}).
	// Idempotent: re-PUTting the same key replaces the bytes. Backs the
	// cloud ToolBlobStore twin — large tool outputs that exceed the
	// inline event preview threshold live here, not in Mongo (they can
	// exceed the 16 MiB BSON document ceiling).
	PutToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) error

	// GetToolBlobRange returns up to `limit` bytes starting at `offset`
	// (limit==0 → all from offset), the full object size, and eof=true
	// when offset+len(data) >= total. offset past the end yields
	// (nil, total, true, nil). Returns ErrArtifactNotFound when the blob
	// is absent so the store layer can map it to an os.ErrNotExist for
	// the paginated HTTP surface.
	GetToolBlobRange(ctx context.Context, runID, toolUseID, kind string, offset, limit int64) (data []byte, total int64, eof bool, err error)

	// DeleteRunToolBlobs removes every blob under `tools/<runID>/` in a
	// single sweep. Best-effort, mirroring DeleteRunAttachments.
	DeleteRunToolBlobs(ctx context.Context, runID string) error

	// PutRunFile uploads a tool-produced artifact file (run report, SBOM,
	// …) under `runfiles/<runID>/<relPath>`. relPath may contain nested
	// segments. Idempotent. Backs the cloud RunFilesStore twin: the runner
	// walks its local scratch dir and streams each file here so large review
	// media never has to be buffered in the runner's memory. size is the exact
	// byte length advertised to the blob backend.
	PutRunFile(ctx context.Context, runID, relPath, contentType string, body io.Reader, size int64) error

	// ListRunFiles enumerates every artifact file under
	// `runfiles/<runID>/` as area-relative paths. Empty slice (no error)
	// when the run produced none.
	ListRunFiles(ctx context.Context, runID string) ([]RunFileObject, error)

	// GetRunFile streams one artifact file. Callers must Close the reader.
	// Returns ErrArtifactNotFound when the (runID, relPath) is absent.
	GetRunFile(ctx context.Context, runID, relPath string) (io.ReadCloser, RunFileObject, error)

	// DeleteRunFiles removes every blob under `runfiles/<runID>/` in a
	// single sweep. Best-effort, mirroring DeleteRunAttachments.
	DeleteRunFiles(ctx context.Context, runID string) error

	// PutIRBlob stashes an out-of-band compiled IR under `ir/<runID>.json`
	// (the queue IRRef fallback for workflows whose IR exceeds the NATS
	// max_payload). Idempotent: re-PUTting the same run replaces the bytes.
	PutIRBlob(ctx context.Context, runID string, body []byte) error

	// GetIRBlob returns the IR bytes previously PUT for a run, addressed by
	// the storage key carried on queue.IRRef.StorageKey. Returns
	// ErrArtifactNotFound when the key is absent. The key is validated
	// against the canonical `ir/<run_id>.json` shape so a tampered
	// reference can never widen the key space.
	GetIRBlob(ctx context.Context, storageKey string) ([]byte, error)

	// DeleteRunIR removes the `ir/<runID>.json` blob. Best-effort, part of
	// the per-run cleanup sweep alongside DeleteRun / DeleteRunToolBlobs.
	// Idempotent: deleting a non-existent key returns nil.
	DeleteRunIR(ctx context.Context, runID string) error
}

// IRBlobKey returns the canonical layout key for an out-of-band compiled
// IR: `ir/<run_id>.json`. This is the value stamped on
// queue.IRRef.StorageKey.
func IRBlobKey(runID string) (string, error) {
	return irBlobKey(runID)
}

// RunFileObject is the metadata the blob backend reports for one
// tool-produced artifact file. Path is area-relative (never absolute,
// never leading "/"), matching store.RunFileInfo.Path.
type RunFileObject struct {
	Path       string
	Size       int64
	ModifiedAt time.Time
}

// AttachmentMeta describes the bytes returned by GetAttachment as
// observed by the storage backend (independent from the
// store.AttachmentRecord which captures the upload-time decision).
type AttachmentMeta struct {
	ContentType  string
	Size         int64
	LastModified time.Time
}

// Config carries the connection settings shared by every Client
// implementation. The S3 impl reads it directly; a future
// in-memory test impl ignores most fields.
type Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	UsePathStyle    bool
}

// ArtifactKey returns the canonical layout key for an artifact. Using
// a single helper here keeps the layout decision in one place — any
// future change (e.g. sharding by run prefix for high cardinality
// stores) only mutates this function. See plan §D.6.
//
// Returns an error when runID or nodeID fails the standard path
// component sanitisation (separators, traversal, control chars), or
// when version is negative.
func ArtifactKey(runID, nodeID string, version int) (string, error) {
	return artifactKey(runID, nodeID, version)
}

// AttachmentKey returns the canonical layout key for an attachment.
// Format: `attachments/<run_id>/<name>/<filename>`. The same shape
// used by the filesystem backend (relative to the store root) so
// migration tooling can copy bytes between backends without
// rewriting paths.
//
// Returns an error when any of the three components fail
// sanitisation.
func AttachmentKey(runID, name, filename string) (string, error) {
	return attachmentKey(runID, name, filename)
}

// AttachmentRunPrefix is the S3 key prefix that contains every
// attachment for a run. Used by DeleteRunAttachments and
// retention sweepers.
//
// Returns an error when runID fails sanitisation.
func AttachmentRunPrefix(runID string) (string, error) {
	return attachmentRunPrefix(runID)
}

// ToolBlobKey returns the canonical layout key for a per-tool-call I/O
// body: `tools/<run_id>/<tool_use_id>/<kind>` (kind ∈ {input,output}).
// Same shape as the filesystem backend's runs/<id>/tools/… so migration
// tooling copies bytes across without rewriting paths.
func ToolBlobKey(runID, toolUseID, kind string) (string, error) {
	return toolBlobKey(runID, toolUseID, kind)
}

// ToolBlobRunPrefix is the S3 key prefix that contains every tool blob
// for a run. Used by DeleteRunToolBlobs and retention sweepers.
func ToolBlobRunPrefix(runID string) (string, error) {
	return toolBlobRunPrefix(runID)
}

// RunFileKey returns the canonical layout key for a tool-produced
// artifact file: `runfiles/<run_id>/<rel_path>`. rel_path may be
// multi-segment; each segment is sanitised and traversal is rejected.
func RunFileKey(runID, relPath string) (string, error) {
	return runFileKey(runID, relPath)
}

// RunFileRunPrefix is the S3 key prefix that contains every artifact
// file for a run. Used by DeleteRunFiles and retention sweepers.
func RunFileRunPrefix(runID string) (string, error) {
	return runFileRunPrefix(runID)
}
