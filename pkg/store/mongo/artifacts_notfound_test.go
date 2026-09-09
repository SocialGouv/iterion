package mongo

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The cloud twin must answer "absent" the same way the filesystem one does,
// or the artifact-contract gate cannot tell a missing version from an
// object-store outage — and an enforce run would admit everything for the
// duration of a blip. Reads only the blob, so it needs no Mongo and runs in
// the ordinary suite rather than only in the gated conformance job.
func TestLoadArtifactWrapsNotFound(t *testing.T) {
	s := &Store{blob: newInMemoryBlob()}
	_, err := s.LoadArtifact(context.Background(), "run_x", "node_a", 7)
	if !errors.Is(err, store.ErrArtifactNotFound) {
		t.Fatalf("LoadArtifact of an absent version = %v, want it to wrap store.ErrArtifactNotFound", err)
	}
}
