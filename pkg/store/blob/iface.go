// Package blob defines the artifact-blob interface implemented by S3
// (cloud) and (potentially) a local filesystem variant for testing.
//
// The S3 implementation lands in plan §F T-16. This file is the
// minimum surface area the Mongo store needs to compile against
// without forcing the AWS SDK as a dependency on every CLI build.
package blob

import "context"

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
func ArtifactKey(runID, nodeID string, version int) string {
	return artifactKey(runID, nodeID, version)
}
