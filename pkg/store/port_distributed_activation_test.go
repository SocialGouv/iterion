package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func distributedTransitionFixture(t *testing.T, root *FilesystemRunStore, expected uint64) (*PortDistributedProof, *PortActivation) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	snapshot := []byte(`{"epoch":3,"observation_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	digest := sha256.Sum256(snapshot)
	proofRevision := expected + 1
	policyRevision := expected + 1
	proof := &PortDistributedProof{Version: PortDistributedProofVersion, PolicyRevision: policyRevision,
		ProofRevision: proofRevision, AuthorityEpoch: 3, StoreIdentity: "filesystem:" + root.Root(),
		ProofDigest: strings.Repeat("a", 64), ObservationDigest: strings.Repeat("a", 64),
		SnapshotDigest: hex.EncodeToString(digest[:]), Snapshot: snapshot,
		VerifiedAt: now, ExpiresAt: now.Add(30 * time.Second)}
	activation := &PortActivation{Version: PortActivationVersion, Revision: policyRevision,
		ProofRevision: proofRevision, Enabled: true, Scope: PortActivationDistributed,
		StoreIdentity: proof.StoreIdentity, ProofDigest: proof.ProofDigest,
		CapabilityDigest: strings.Repeat("b", 64), QueueVersion: 15,
		ConsumerAccessEvidence: "verified deployment authority observation", VerifiedAt: now,
		ExpiresAt: now.Add(30 * time.Second)}
	return proof, activation
}

func TestFilesystemDistributedActivationPublishesProofAndPolicyTogether(t *testing.T) {
	filesystem, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proof, activation := distributedTransitionFixture(t, filesystem, 0)
	proof.Snapshot = append([]byte(" \n"), append(proof.Snapshot, []byte("\n ")...)...)
	if err := filesystem.SavePortDistributedActivation(t.Context(), 0, proof, activation); err != nil {
		t.Fatal(err)
	}
	// The writer stores the canonical bytes whose digest is signed, even when
	// an administrative caller supplied equivalent JSON with surrounding
	// whitespace.
	loadedProof, err := filesystem.LoadPortDistributedProof(t.Context())
	if err != nil || loadedProof == nil || loadedProof.PolicyRevision != 1 || !bytes.Equal(loadedProof.Snapshot, bytes.TrimSpace(proof.Snapshot)) {
		t.Fatalf("proof after atomic publication = %+v, %v", loadedProof, err)
	}
	loadedActivation, err := filesystem.LoadPortActivation(t.Context())
	if err != nil || loadedActivation == nil || loadedActivation.Revision != 1 {
		t.Fatalf("activation after atomic publication = %+v, %v", loadedActivation, err)
	}

	// A new policy cannot reuse the proof revision from the previous policy.
	staleProof, staleActivation := distributedTransitionFixture(t, filesystem, 1)
	staleProof.ProofRevision = proof.ProofRevision
	staleActivation.ProofRevision = staleProof.ProofRevision
	if err := filesystem.SavePortDistributedActivation(t.Context(), 1, staleProof, staleActivation); !errors.Is(err, ErrRunConflict) {
		t.Fatalf("non-advancing proof revision returned %v", err)
	}

	nextProof, nextActivation := distributedTransitionFixture(t, filesystem, 0)
	nextProof.ProofDigest = strings.Repeat("c", 64)
	nextActivation.ProofDigest = nextProof.ProofDigest
	if err := filesystem.SavePortDistributedActivation(t.Context(), 0, nextProof, nextActivation); !errors.Is(err, ErrRunConflict) {
		t.Fatalf("stale combined publication returned %v", err)
	}
	stillProof, err := filesystem.LoadPortDistributedProof(t.Context())
	if err != nil || stillProof.PolicyRevision != 1 || stillProof.ProofDigest != loadedProof.ProofDigest {
		t.Fatalf("stale combined publication changed proof: %+v, %v", stillProof, err)
	}
	stillActivation, err := filesystem.LoadPortActivation(t.Context())
	if err != nil || stillActivation.Revision != 1 || !stillActivation.Enabled {
		t.Fatalf("stale combined publication changed activation: %+v, %v", stillActivation, err)
	}
}

func TestFilesystemDistributedActivationRecoversDurableJournal(t *testing.T) {
	filesystem, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	firstProof, firstActivation := distributedTransitionFixture(t, filesystem, 0)
	if err := filesystem.SavePortDistributedActivation(t.Context(), 0, firstProof, firstActivation); err != nil {
		t.Fatal(err)
	}
	nextProof, nextActivation := distributedTransitionFixture(t, filesystem, 1)
	txn := portDistributedActivationTxn{Version: 1, ExpectedPolicy: 1, Proof: *nextProof, Activation: *nextActivation}
	raw, err := json.Marshal(txn)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(filesystem.portDistributedActivationTxnPath(), raw, filePerm); err != nil {
		t.Fatal(err)
	}
	loaded, err := filesystem.LoadPortActivation(t.Context())
	if err != nil || loaded == nil || loaded.Revision != 2 {
		t.Fatalf("journal was not recovered: %+v, %v", loaded, err)
	}
	proof, err := filesystem.LoadPortDistributedProof(t.Context())
	if err != nil || proof == nil || proof.PolicyRevision != 2 {
		t.Fatalf("journal proof was not recovered: %+v, %v", proof, err)
	}
	if _, err := os.Stat(filesystem.portDistributedActivationTxnPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered journal remains on disk: %v", err)
	}

	// A second read is idempotent and must not rewrite either target.
	activationBytes, _ := os.ReadFile(filesystem.portActivationPath())
	proofBytes, _ := os.ReadFile(filesystem.portDistributedProofPath())
	if _, err := filesystem.LoadPortActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	activationAgain, _ := os.ReadFile(filesystem.portActivationPath())
	proofAgain, _ := os.ReadFile(filesystem.portDistributedProofPath())
	if !bytes.Equal(activationBytes, activationAgain) || !bytes.Equal(proofBytes, proofAgain) {
		t.Fatal("journal recovery was not idempotent")
	}
}

func TestFilesystemDistributedActivationRefreshFencingAndDisable(t *testing.T) {
	filesystem, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proof, activation := distributedTransitionFixture(t, filesystem, 0)
	if err := filesystem.SavePortDistributedActivation(t.Context(), 0, proof, activation); err != nil {
		t.Fatal(err)
	}
	lease := PortActivationRefreshLease{Owner: "server-a", Token: 1, ExpiresAt: time.Now().UTC().Add(30 * time.Second)}
	claimed, err := filesystem.ClaimPortActivationRefresh(t.Context(), 1, lease)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed.ExpiresAt.Equal(activation.ExpiresAt) || claimed.ProofRevision != activation.ProofRevision {
		t.Fatal("claim changed proof freshness")
	}
	now := time.Now().UTC()
	renewed, err := filesystem.RenewPortActivation(t.Context(), PortActivationRenewal{
		PolicyRevision: 1, ProofRevision: claimed.ProofRevision, ProofDigest: claimed.ProofDigest,
		Lease: lease, VerifiedAt: now, ExpiresAt: now.Add(30 * time.Second)})
	if err != nil || renewed.ProofRevision != 2 || renewed.Revision != 1 {
		t.Fatalf("renewed activation = %+v, %v", renewed, err)
	}
	if _, err := filesystem.RenewPortActivation(t.Context(), PortActivationRenewal{
		PolicyRevision: 1, ProofRevision: claimed.ProofRevision, ProofDigest: claimed.ProofDigest,
		Lease: lease, VerifiedAt: now, ExpiresAt: now.Add(30 * time.Second)}); !errors.Is(err, ErrRunConflict) {
		t.Fatalf("stale renewal returned %v", err)
	}
	disabled, err := filesystem.DisablePortActivation(t.Context(), 1)
	if err != nil || disabled.Enabled || disabled.Revision != 2 || disabled.RefreshLease != nil || disabled.ProofRevision != 2 {
		t.Fatalf("disabled activation = %+v, %v", disabled, err)
	}
	if _, err := filesystem.ClaimPortActivationRefresh(t.Context(), 2, PortActivationRefreshLease{
		Owner: "server-b", Token: 2, ExpiresAt: time.Now().UTC().Add(30 * time.Second)}); !errors.Is(err, ErrRunConflict) {
		t.Fatalf("disabled policy accepted a refresh claim: %v", err)
	}
}

func TestFilesystemDistributedRenewalPublishesProofAndActivationTogether(t *testing.T) {
	filesystem, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proof, activation := distributedTransitionFixture(t, filesystem, 0)
	if err := filesystem.SavePortDistributedActivation(t.Context(), 0, proof, activation); err != nil {
		t.Fatal(err)
	}
	lease := PortActivationRefreshLease{Owner: "server-a", Token: 1, ExpiresAt: time.Now().UTC().Add(30 * time.Second)}
	claimed, err := filesystem.ClaimPortActivationRefresh(t.Context(), 1, lease)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	currentProof, err := filesystem.LoadPortDistributedProof(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	nextProof := *currentProof
	nextProof.ProofRevision = claimed.ProofRevision + 1
	nextProof.VerifiedAt = now
	nextProof.ExpiresAt = now.Add(30 * time.Second)
	renewal := PortActivationRenewal{PolicyRevision: claimed.Revision, ProofRevision: claimed.ProofRevision,
		ProofDigest: claimed.ProofDigest, Lease: *claimed.RefreshLease, VerifiedAt: now, ExpiresAt: nextProof.ExpiresAt}
	wrongIdentity := nextProof
	wrongIdentity.StoreIdentity = "filesystem:foreign"
	if _, err := filesystem.RenewPortActivationWithProof(t.Context(), renewal, &wrongIdentity); !errors.Is(err, ErrPortActivation) {
		t.Fatalf("renewal with a foreign proof identity returned %v", err)
	}
	next, err := filesystem.RenewPortActivationWithProof(t.Context(), renewal, &nextProof)
	if err != nil || next == nil || next.ProofRevision != claimed.ProofRevision+1 {
		t.Fatalf("atomic renewal = %+v, %v", next, err)
	}
	loadedProof, err := filesystem.LoadPortDistributedProof(t.Context())
	if err != nil || loadedProof.ProofRevision != next.ProofRevision || !loadedProof.ExpiresAt.Equal(next.ExpiresAt) {
		t.Fatalf("renewed proof = %+v, activation = %+v, err = %v", loadedProof, next, err)
	}
	loadedActivation, err := filesystem.LoadPortActivation(t.Context())
	if err != nil || loadedActivation.ProofRevision != next.ProofRevision || !loadedActivation.ExpiresAt.Equal(next.ExpiresAt) {
		t.Fatalf("renewed activation = %+v, err = %v", loadedActivation, err)
	}
	if _, err := filesystem.RenewPortActivationWithProof(t.Context(), renewal, &nextProof); !errors.Is(err, ErrRunConflict) {
		t.Fatalf("stale atomic renewal returned %v", err)
	}
}

func TestFilesystemDistributedRenewalRecoversDurableJournal(t *testing.T) {
	filesystem, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proof, activation := distributedTransitionFixture(t, filesystem, 0)
	if err := filesystem.SavePortDistributedActivation(t.Context(), 0, proof, activation); err != nil {
		t.Fatal(err)
	}
	lease := PortActivationRefreshLease{Owner: "server-a", Token: 1, ExpiresAt: time.Now().UTC().Add(30 * time.Second)}
	claimed, err := filesystem.ClaimPortActivationRefresh(t.Context(), 1, lease)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	currentProof, err := filesystem.LoadPortDistributedProof(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	nextProof := *currentProof
	nextProof.ProofRevision = claimed.ProofRevision + 1
	nextProof.VerifiedAt = now
	nextProof.ExpiresAt = now.Add(30 * time.Second)
	renewal := PortActivationRenewal{PolicyRevision: claimed.Revision, ProofRevision: claimed.ProofRevision,
		ProofDigest: claimed.ProofDigest, Lease: *claimed.RefreshLease, VerifiedAt: now, ExpiresAt: nextProof.ExpiresAt}
	nextActivation := *claimed
	nextActivation.ProofRevision = nextProof.ProofRevision
	nextActivation.VerifiedAt = now
	nextActivation.ExpiresAt = nextProof.ExpiresAt
	txn := portDistributedRenewalTxn{Version: 1, Renewal: renewal, Proof: nextProof, Activation: nextActivation}
	raw, err := json.Marshal(txn)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(filesystem.portDistributedRenewalTxnPath(), raw, filePerm); err != nil {
		t.Fatal(err)
	}
	loaded, err := filesystem.LoadPortActivation(t.Context())
	if err != nil || loaded.ProofRevision != nextProof.ProofRevision {
		t.Fatalf("renewal journal was not recovered: %+v, %v", loaded, err)
	}
	loadedProof, err := filesystem.LoadPortDistributedProof(t.Context())
	if err != nil || loadedProof.ProofRevision != nextProof.ProofRevision {
		t.Fatalf("renewal journal proof was not recovered: %+v, %v", loadedProof, err)
	}
	if _, err := os.Stat(filesystem.portDistributedRenewalTxnPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered renewal journal remains on disk: %v", err)
	}
}
