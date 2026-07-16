package store

import "context"

// IRBlobStore is an optional interface a store implements to stash an
// oversized compiled IR out-of-band, keyed by run id. It is the storage
// half of the queue's IRRef fallback: when a workflow's marshaled IR
// exceeds the NATS max_payload (default 1 MiB), the cloud publisher parks
// the IR here and sends a lightweight queue.IRRef on the RunMessage; the
// runner fetches it back before compiling.
//
// The cloud (Mongo) store satisfies it, backed by the same S3 blob client
// that holds artifacts + tool blobs (IR blobs can exceed the 16 MiB BSON
// ceiling, so they live in S3, not Mongo). Filesystem / local stores do
// NOT implement it — the local runtime never crosses the queue, so an
// oversized-IR offload cannot arise there. Callers MUST nil-check via
// AsIRBlobStore.
type IRBlobStore interface {
	// PutIRBlob stashes body under a deterministic key derived from
	// runID and returns that key (which travels on queue.IRRef.StorageKey).
	// Idempotent: re-writing the same run replaces the prior bytes.
	PutIRBlob(ctx context.Context, runID string, body []byte) (storageKey string, err error)
	// GetIRBlob returns the bytes previously stashed under storageKey.
	// A missing blob returns an os.ErrNotExist-compatible error.
	GetIRBlob(ctx context.Context, storageKey string) ([]byte, error)
	// IRBlobBackend names the backend the blobs live in ("s3" | "mongo"),
	// matching the queue.IRBackend enum the publisher stamps on the IRRef.
	IRBlobBackend() string
}

// AsIRBlobStore returns s as IRBlobStore when the backend can host
// out-of-band IR blobs, or nil otherwise. Callers MUST nil-check
// (filesystem / local stores return nil).
func AsIRBlobStore(s RunStore) IRBlobStore {
	if s == nil {
		return nil
	}
	b, _ := s.(IRBlobStore)
	return b
}
