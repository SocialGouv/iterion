package mongo

import (
	"context"
	"errors"
	"os"
	"testing"
)

// The IRBlobStore methods only touch the S3 blob backend (not Mongo), so a
// bare *Store with an in-memory blob exercises the real cloud impl without
// a live database.

func TestStoreIRBlob_RoundTrip(t *testing.T) {
	s := &Store{blob: newInMemoryBlob()}
	ctx := context.Background()
	body := []byte(`{"nodes":[{"id":"a"}]}`)
	key, err := s.PutIRBlob(ctx, "run-1", body)
	if err != nil {
		t.Fatalf("PutIRBlob: %v", err)
	}
	if key != "ir/run-1.json" {
		t.Fatalf("key = %q, want ir/run-1.json", key)
	}
	got, err := s.GetIRBlob(ctx, key)
	if err != nil {
		t.Fatalf("GetIRBlob: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, body)
	}
	if s.IRBlobBackend() != "s3" {
		t.Fatalf("backend = %q, want s3", s.IRBlobBackend())
	}
}

func TestStoreIRBlob_NotFoundIsErrNotExist(t *testing.T) {
	s := &Store{blob: newInMemoryBlob()}
	_, err := s.GetIRBlob(context.Background(), "ir/absent.json")
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}
