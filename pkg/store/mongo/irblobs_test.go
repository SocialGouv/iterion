package mongo

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// The IRBlobStore's BLOB-layer contract, exercised on a bare *Store with
// an in-memory blob: the key is canonical (ir/<run_id>.json), the
// round-trip preserves the bytes, and the backend is S3. The READ itself
// is tenant-mediated at the store level (see irblobs_mediation_test.go,
// gated on ITERION_TEST_MONGO_URI — the mediation loads the run the key
// addresses under the caller's tenant filter, which needs the runs
// collection).

func TestStoreIRBlob_RoundTrip(t *testing.T) {
	b := newInMemoryBlob()
	ctx := context.Background()
	body := []byte(`{"nodes":[{"id":"a"}]}`)
	key, err := blob.IRBlobKey("run-1")
	if err != nil {
		t.Fatalf("IRBlobKey: %v", err)
	}
	if err := b.PutIRBlob(ctx, "run-1", body); err != nil {
		t.Fatalf("PutIRBlob: %v", err)
	}
	got, err := b.GetIRBlob(ctx, key)
	if err != nil {
		t.Fatalf("GetIRBlob: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, body)
	}
	s := &Store{blob: b}
	if s.IRBlobBackend() != "s3" {
		t.Fatalf("backend = %q, want s3", s.IRBlobBackend())
	}
}

func TestStoreIRBlob_NotFoundIsErrNotExist(t *testing.T) {
	b := newInMemoryBlob()
	if _, err := b.GetIRBlob(context.Background(), "ir/absent.json"); !errors.Is(err, blob.ErrArtifactNotFound) {
		t.Fatalf("expected blob.ErrArtifactNotFound, got %v", err)
	}
}
