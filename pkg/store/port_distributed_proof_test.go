package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFilesystemDistributedProofIsBoundAndPrivate(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	snapshot := []byte(`{"version":1,"queue_client":{"account":"WORK","principal":"worker-user"}}`)
	digest := sha256.Sum256(snapshot)
	proof := &PortDistributedProof{Version: PortDistributedProofVersion, PolicyRevision: 1,
		ProofRevision: 1, AuthorityEpoch: 3, StoreIdentity: "mongodb:fixture",
		ProofDigest: strings.Repeat("a", 64), ObservationDigest: strings.Repeat("b", 64),
		SnapshotDigest: hex.EncodeToString(digest[:]), Snapshot: snapshot,
		VerifiedAt: now, ExpiresAt: now.Add(30 * time.Second)}
	if err := s.SavePortDistributedProof(t.Context(), 0, proof); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadPortDistributedProof(t.Context())
	if err != nil || loaded == nil || loaded.SnapshotDigest != proof.SnapshotDigest || string(loaded.Snapshot) != string(snapshot) {
		t.Fatalf("proof snapshot did not round-trip: %+v %v", loaded, err)
	}
	if err := s.SavePortDistributedProof(t.Context(), 1, &PortDistributedProof{Version: PortDistributedProofVersion}); !errors.Is(err, ErrPortActivation) {
		t.Fatalf("malformed proof write was accepted: %v", err)
	}
	loaded.SnapshotDigest = strings.Repeat("c", 64)
	if err := loaded.Validate(); !errors.Is(err, ErrPortActivation) {
		t.Fatalf("tampered proof digest was accepted: %v", err)
	}
}
