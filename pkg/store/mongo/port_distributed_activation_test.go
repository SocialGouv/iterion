package mongo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestNativeActivationMongoPublishesProofAndPolicyAtomically(t *testing.T) {
	s := nativeNamespaceStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	snapshot := []byte(`{"epoch":3,"observation_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	digest := sha256.Sum256(snapshot)
	identity := s.PortBackendIdentity()
	proof := &store.PortDistributedProof{Version: store.PortDistributedProofVersion, PolicyRevision: 1,
		ProofRevision: 1, AuthorityEpoch: 3, StoreIdentity: identity,
		ProofDigest: strings.Repeat("a", 64), ObservationDigest: strings.Repeat("a", 64),
		SnapshotDigest: hex.EncodeToString(digest[:]), Snapshot: snapshot,
		VerifiedAt: now, ExpiresAt: now.Add(30 * time.Second)}
	activation := &store.PortActivation{Version: store.PortActivationVersion, Revision: 1,
		ProofRevision: 1, Enabled: true, Scope: store.PortActivationDistributed,
		StoreIdentity: identity, ProofDigest: proof.ProofDigest,
		CapabilityDigest: strings.Repeat("b", 64), QueueVersion: queue.SchemaVersion,
		ConsumerAccessEvidence: "verified deployment authority observation", VerifiedAt: now,
		ExpiresAt: now.Add(30 * time.Second)}
	if err := s.SavePortDistributedActivation(t.Context(), 0, proof, activation); err != nil {
		t.Fatal(err)
	}
	loadedProof, err := s.LoadPortDistributedProof(t.Context())
	if err != nil || loadedProof == nil || loadedProof.PolicyRevision != 1 {
		t.Fatalf("proof after atomic publication = %+v, %v", loadedProof, err)
	}
	loadedActivation, err := s.LoadPortActivation(t.Context())
	if err != nil || loadedActivation == nil || loadedActivation.Revision != 1 {
		t.Fatalf("activation after atomic publication = %+v, %v", loadedActivation, err)
	}
	proof.ProofDigest = strings.Repeat("c", 64)
	activation.ProofDigest = proof.ProofDigest
	if err := s.SavePortDistributedActivation(t.Context(), 0, proof, activation); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("stale atomic publication returned %v", err)
	}
	stillProof, err := s.LoadPortDistributedProof(t.Context())
	if err != nil || stillProof.ProofDigest != strings.Repeat("a", 64) {
		t.Fatalf("stale atomic publication changed proof: %+v, %v", stillProof, err)
	}
	stillActivation, err := s.LoadPortActivation(t.Context())
	if err != nil || stillActivation.Revision != 1 || !stillActivation.Enabled {
		t.Fatalf("stale atomic publication changed activation: %+v, %v", stillActivation, err)
	}
}

func TestNativeActivationMongoRenewsProofAndPolicyAtomically(t *testing.T) {
	s := nativeNamespaceStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	snapshot := []byte(`{"epoch":3,"observation_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	digest := sha256.Sum256(snapshot)
	identity := s.PortBackendIdentity()
	proof := &store.PortDistributedProof{Version: store.PortDistributedProofVersion, PolicyRevision: 1,
		ProofRevision: 1, AuthorityEpoch: 3, StoreIdentity: identity,
		ProofDigest: strings.Repeat("a", 64), ObservationDigest: strings.Repeat("a", 64),
		SnapshotDigest: hex.EncodeToString(digest[:]), Snapshot: snapshot,
		VerifiedAt: now, ExpiresAt: now.Add(30 * time.Second)}
	activation := &store.PortActivation{Version: store.PortActivationVersion, Revision: 1,
		ProofRevision: 1, Enabled: true, Scope: store.PortActivationDistributed,
		StoreIdentity: identity, ProofDigest: proof.ProofDigest,
		CapabilityDigest: strings.Repeat("b", 64), QueueVersion: queue.SchemaVersion,
		ConsumerAccessEvidence: "verified deployment authority observation", VerifiedAt: now,
		ExpiresAt: now.Add(30 * time.Second)}
	if err := s.SavePortDistributedActivation(t.Context(), 0, proof, activation); err != nil {
		t.Fatal(err)
	}
	lease := store.PortActivationRefreshLease{Owner: "server-a", Token: 1, ExpiresAt: time.Now().UTC().Add(30 * time.Second).Truncate(time.Millisecond)}
	claimed, err := s.ClaimPortActivationRefresh(t.Context(), 1, lease)
	if err != nil {
		t.Fatal(err)
	}
	verified := time.Now().UTC().Truncate(time.Millisecond)
	nextProof := *proof
	nextProof.ProofRevision = claimed.ProofRevision + 1
	nextProof.VerifiedAt = verified
	nextProof.ExpiresAt = verified.Add(30 * time.Second)
	renewal := store.PortActivationRenewal{PolicyRevision: claimed.Revision, ProofRevision: claimed.ProofRevision,
		ProofDigest: claimed.ProofDigest, Lease: *claimed.RefreshLease, VerifiedAt: verified, ExpiresAt: nextProof.ExpiresAt}
	next, err := s.RenewPortActivationWithProof(t.Context(), renewal, &nextProof)
	if err != nil || next == nil || next.ProofRevision != 2 || next.Revision != 1 {
		t.Fatalf("atomic Mongo renewal = %+v, %v", next, err)
	}
	loadedProof, err := s.LoadPortDistributedProof(t.Context())
	if err != nil || loadedProof.ProofRevision != next.ProofRevision || !loadedProof.ExpiresAt.Equal(next.ExpiresAt) {
		t.Fatalf("renewed Mongo proof = %+v, activation = %+v, err = %v", loadedProof, next, err)
	}
	loadedActivation, err := s.LoadPortActivation(t.Context())
	if err != nil || loadedActivation.ProofRevision != next.ProofRevision || !loadedActivation.ExpiresAt.Equal(next.ExpiresAt) {
		t.Fatalf("renewed Mongo activation = %+v, err = %v", loadedActivation, err)
	}
}

func TestNativeActivationMongoReactivatesAfterDisableWithRetainedProof(t *testing.T) {
	s := nativeNamespaceStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	snapshot := []byte(`{"epoch":3,"observation_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	digest := sha256.Sum256(snapshot)
	identity := s.PortBackendIdentity()
	proof := &store.PortDistributedProof{Version: store.PortDistributedProofVersion, PolicyRevision: 1,
		ProofRevision: 1, AuthorityEpoch: 3, StoreIdentity: identity,
		ProofDigest: strings.Repeat("a", 64), ObservationDigest: strings.Repeat("a", 64),
		SnapshotDigest: hex.EncodeToString(digest[:]), Snapshot: snapshot,
		VerifiedAt: now, ExpiresAt: now.Add(30 * time.Second)}
	activation := &store.PortActivation{Version: store.PortActivationVersion, Revision: 1,
		ProofRevision: 1, Enabled: true, Scope: store.PortActivationDistributed,
		StoreIdentity: identity, ProofDigest: proof.ProofDigest,
		CapabilityDigest: strings.Repeat("b", 64), QueueVersion: queue.SchemaVersion,
		ConsumerAccessEvidence: "verified deployment authority observation", VerifiedAt: now,
		ExpiresAt: now.Add(30 * time.Second)}
	if err := s.SavePortDistributedActivation(t.Context(), 0, proof, activation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DisablePortActivation(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	nextProof := *proof
	nextProof.PolicyRevision = 3
	nextProof.ProofRevision = 2
	nextProof.ProofDigest = strings.Repeat("c", 64)
	nextActivation := *activation
	nextActivation.Revision = 3
	nextActivation.ProofRevision = 2
	nextActivation.ProofDigest = nextProof.ProofDigest
	nextActivation.VerifiedAt = time.Now().UTC().Truncate(time.Millisecond)
	nextActivation.ExpiresAt = nextActivation.VerifiedAt.Add(30 * time.Second)
	nextProof.VerifiedAt = nextActivation.VerifiedAt
	nextProof.ExpiresAt = nextActivation.ExpiresAt
	if err := s.SavePortDistributedActivation(t.Context(), 2, &nextProof, &nextActivation); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadPortActivation(t.Context())
	if err != nil || loaded.Revision != 3 || !loaded.Enabled {
		t.Fatalf("reactivated Mongo activation = %+v, %v", loaded, err)
	}
	loadedProof, err := s.LoadPortDistributedProof(t.Context())
	if err != nil || loadedProof.PolicyRevision != 3 || loadedProof.ProofDigest != nextProof.ProofDigest {
		t.Fatalf("reactivated Mongo proof = %+v, %v", loadedProof, err)
	}
}
