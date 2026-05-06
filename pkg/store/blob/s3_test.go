package blob

import (
	"context"
	"errors"
	"testing"
)

// NewS3 must reject an empty bucket: the SDK accepts empty string and
// surfaces it later as a 400 BadRequest, which is harder to debug. We
// fail fast at construction.
func TestNewS3RejectsEmptyBucket(t *testing.T) {
	t.Parallel()

	_, err := NewS3(context.Background(), Config{
		Region:          "us-east-1",
		Bucket:          "",
		AccessKeyID:     "key",
		SecretAccessKey: "secret",
	})
	if err == nil {
		t.Fatal("expected error for empty bucket, got nil")
	}
}

// ErrArtifactNotFound is the single sentinel callers match against.
// Make sure the helper that wraps it preserves the relationship so a
// caller's errors.Is keeps working.
func TestErrArtifactNotFoundIsExported(t *testing.T) {
	t.Parallel()

	wrapped := wrapNotFound("artifacts/run-1/node-a/0.json")
	if !errors.Is(wrapped, ErrArtifactNotFound) {
		t.Fatalf("expected wrapped error to match ErrArtifactNotFound, got %v", wrapped)
	}
}

// ArtifactKey is the public re-export of the canonical layout. The
// behaviour is owned by key.go (panics on bad inputs) but we lock the
// happy-path format here so a future refactor can't silently change
// it without breaking the migration tool.
func TestArtifactKeyHappyPath(t *testing.T) {
	t.Parallel()

	got := ArtifactKey("run-001", "agent_a", 3)
	want := "artifacts/run-001/agent_a/3.json"
	if got != want {
		t.Fatalf("ArtifactKey: got %q want %q", got, want)
	}
}

// wrapNotFound exists only as a test-side mirror of how s3.go composes
// the error: keep this in sync with isS3NotFound's call site.
func wrapNotFound(key string) error {
	return errors.Join(ErrArtifactNotFound, errors.New(key))
}
