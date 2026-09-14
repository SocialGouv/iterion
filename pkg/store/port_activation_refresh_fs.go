package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

// DisablePortActivation is the filesystem CAS implementation used by local
// tools and tests. It advances policy independently of the proof revision and
// clears the refresher fence so an old owner cannot renew the disabled policy.
func (s *FilesystemRunStore) DisablePortActivation(ctx context.Context, expectedPolicy uint64) (*PortActivation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if expectedPolicy == 0 || expectedPolicy >= math.MaxInt64 {
		return nil, fmt.Errorf("%w: invalid policy revision", ErrRunConflict)
	}
	activation, proofLock, err := s.distributedActivationLocks()
	if err != nil {
		return nil, err
	}
	defer func() { _ = activation.Unlock() }()
	defer func() { _ = proofLock.Unlock() }()
	if err := s.recoverDistributedStateLocked(ctx); err != nil {
		return nil, err
	}
	current, err := s.loadPortActivationUnlocked(ctx)
	if err != nil {
		return nil, err
	}
	if current == nil || current.Revision != expectedPolicy {
		return nil, fmt.Errorf("%w: activation revision changed", ErrRunConflict)
	}
	next := *current
	next.Revision = expectedPolicy + 1
	next.Enabled = false
	next.RefreshLease = nil
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := writePortActivation(s, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

// ClaimPortActivationRefresh installs only a newer authority fence. It does
// not extend the current proof and therefore cannot keep admission alive by
// itself.
func (s *FilesystemRunStore) ClaimPortActivationRefresh(ctx context.Context, policy uint64,
	lease PortActivationRefreshLease) (*PortActivation, error) {
	now := time.Now().UTC()
	if policy == 0 || policy > math.MaxInt64 || lease.Validate() != nil || !now.Before(lease.ExpiresAt) ||
		lease.ExpiresAt.After(now.Add(PortDistributedProofMaxAge)) {
		return nil, fmt.Errorf("%w: invalid authority refresh lease", ErrPortActivation)
	}
	activation, proofLock, err := s.distributedActivationLocks()
	if err != nil {
		return nil, err
	}
	defer func() { _ = activation.Unlock() }()
	defer func() { _ = proofLock.Unlock() }()
	if err := s.recoverDistributedStateLocked(ctx); err != nil {
		return nil, err
	}
	current, err := s.loadPortActivationUnlocked(ctx)
	if err != nil {
		return nil, err
	}
	if current == nil || !current.Enabled || current.Scope != PortActivationDistributed || current.Revision != policy {
		return nil, fmt.Errorf("%w: activation policy changed", ErrRunConflict)
	}
	if current.RefreshLease != nil && current.RefreshLease.Token >= lease.Token {
		return nil, fmt.Errorf("%w: authority refresh fence changed", ErrRunConflict)
	}
	next := *current
	next.RefreshLease = &lease
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := writePortActivation(s, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

// RenewPortActivation updates only the verified freshness fields after an
// observation whose fingerprint was compared by the authority adapter.
func (s *FilesystemRunStore) RenewPortActivation(ctx context.Context, renewal PortActivationRenewal) (*PortActivation, error) {
	now := time.Now().UTC()
	if err := renewal.Validate(now); err != nil {
		return nil, err
	}
	activation, proofLock, err := s.distributedActivationLocks()
	if err != nil {
		return nil, err
	}
	defer func() { _ = activation.Unlock() }()
	defer func() { _ = proofLock.Unlock() }()
	if err := s.recoverDistributedStateLocked(ctx); err != nil {
		return nil, err
	}
	current, err := s.loadPortActivationUnlocked(ctx)
	if err != nil {
		return nil, err
	}
	if current == nil || !current.Enabled || current.Scope != PortActivationDistributed ||
		current.Revision != renewal.PolicyRevision || current.ProofRevision != renewal.ProofRevision ||
		current.ProofDigest != renewal.ProofDigest || current.RefreshLease == nil ||
		current.RefreshLease.Owner != renewal.Lease.Owner || current.RefreshLease.Token != renewal.Lease.Token ||
		!current.RefreshLease.ExpiresAt.Equal(renewal.Lease.ExpiresAt) || !now.Before(current.ExpiresAt) ||
		current.VerifiedAt.After(renewal.VerifiedAt) || renewal.ProofRevision >= math.MaxInt64 {
		return nil, fmt.Errorf("%w: activation policy, proof or authority lease changed", ErrRunConflict)
	}
	next := *current
	next.ProofRevision = renewal.ProofRevision + 1
	next.VerifiedAt = renewal.VerifiedAt
	next.ExpiresAt = renewal.ExpiresAt
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := writePortActivation(s, &next); err != nil {
		return nil, err
	}
	return &next, nil
}

type portDistributedRenewalTxn struct {
	Version    int                   `json:"version"`
	Renewal    PortActivationRenewal `json:"renewal"`
	Proof      PortDistributedProof  `json:"proof"`
	Activation PortActivation        `json:"activation"`
}

func validatePortDistributedRenewalPayload(txn portDistributedRenewalTxn) error {
	if txn.Version != 1 || txn.Renewal.PolicyRevision == 0 || txn.Renewal.ProofRevision == 0 ||
		txn.Renewal.ProofRevision >= math.MaxInt64 || txn.Renewal.ProofDigest == "" ||
		txn.Renewal.Lease.Validate() != nil || txn.Renewal.VerifiedAt.IsZero() || txn.Renewal.ExpiresAt.IsZero() ||
		txn.Proof.Validate() != nil || txn.Activation.Validate() != nil {
		return fmt.Errorf("%w: malformed distributed renewal transaction", ErrPortActivation)
	}
	if txn.Proof.PolicyRevision != txn.Renewal.PolicyRevision || txn.Proof.ProofRevision != txn.Renewal.ProofRevision+1 ||
		txn.Proof.ProofDigest != txn.Renewal.ProofDigest || !txn.Proof.VerifiedAt.Equal(txn.Renewal.VerifiedAt) ||
		!txn.Proof.ExpiresAt.Equal(txn.Renewal.ExpiresAt) || txn.Activation.Revision != txn.Renewal.PolicyRevision ||
		txn.Activation.ProofRevision != txn.Proof.ProofRevision || txn.Activation.ProofDigest != txn.Proof.ProofDigest ||
		txn.Activation.StoreIdentity != txn.Proof.StoreIdentity || txn.Activation.Scope != PortActivationDistributed ||
		!txn.Activation.Enabled || txn.Activation.RefreshLease == nil ||
		txn.Activation.RefreshLease.Owner != txn.Renewal.Lease.Owner || txn.Activation.RefreshLease.Token != txn.Renewal.Lease.Token ||
		!txn.Activation.RefreshLease.ExpiresAt.Equal(txn.Renewal.Lease.ExpiresAt) ||
		!txn.Activation.VerifiedAt.Equal(txn.Renewal.VerifiedAt) || !txn.Activation.ExpiresAt.Equal(txn.Renewal.ExpiresAt) {
		return fmt.Errorf("%w: distributed renewal transaction does not describe one transition", ErrPortActivation)
	}
	return nil
}

func sameRefreshLease(a *PortActivationRefreshLease, b PortActivationRefreshLease) bool {
	return a != nil && a.Owner == b.Owner && a.Token == b.Token && a.ExpiresAt.Equal(b.ExpiresAt)
}

func activationBeforeRenewal(a *PortActivation, r PortActivationRenewal) bool {
	return a != nil && a.Version == PortActivationVersion && a.Revision == r.PolicyRevision &&
		a.ProofRevision == r.ProofRevision && a.ProofDigest == r.ProofDigest && a.Enabled &&
		a.Scope == PortActivationDistributed && sameRefreshLease(a.RefreshLease, r.Lease) &&
		!a.ExpiresAt.IsZero() && !a.VerifiedAt.IsZero()
}

func activationAfterRenewal(a *PortActivation, expected PortActivation) bool {
	return a != nil && a.Version == expected.Version && a.Revision == expected.Revision &&
		a.ProofRevision == expected.ProofRevision && a.ProofDigest == expected.ProofDigest && a.Enabled &&
		a.Scope == expected.Scope && a.StoreIdentity == expected.StoreIdentity &&
		sameRefreshLease(a.RefreshLease, *expected.RefreshLease) && a.VerifiedAt.Equal(expected.VerifiedAt) &&
		a.ExpiresAt.Equal(expected.ExpiresAt)
}

func proofMatchesRenewal(p *PortDistributedProof, r PortActivationRenewal, storeIdentity string, revision uint64) bool {
	return p != nil && p.PolicyRevision == r.PolicyRevision && p.ProofRevision == revision &&
		p.ProofDigest == r.ProofDigest && p.StoreIdentity == storeIdentity
}

func (s *FilesystemRunStore) recoverDistributedRenewalLocked(ctx context.Context) error {
	raw, err := os.ReadFile(s.portDistributedRenewalTxnPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var txn portDistributedRenewalTxn
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&txn); err != nil || decoder.Decode(new(any)) != io.EOF || validatePortDistributedRenewalPayload(txn) != nil {
		return fmt.Errorf("%w: unreadable distributed renewal transaction", ErrPortActivation)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	currentActivation, err := s.loadPortActivationUnlocked(ctx)
	if err != nil {
		return err
	}
	currentProof, err := s.loadPortDistributedProofUnlocked(ctx)
	if err != nil {
		return err
	}
	if currentActivation == nil || currentActivation.StoreIdentity != txn.Proof.StoreIdentity {
		return fmt.Errorf("%w: distributed renewal transaction has no matching activation", ErrRunConflict)
	}
	before := activationBeforeRenewal(currentActivation, txn.Renewal)
	after := activationAfterRenewal(currentActivation, txn.Activation)
	oldProof := proofMatchesRenewal(currentProof, txn.Renewal, txn.Proof.StoreIdentity, txn.Renewal.ProofRevision)
	newProof := proofMatchesRenewal(currentProof, txn.Renewal, txn.Proof.StoreIdentity, txn.Proof.ProofRevision)
	if !before && !after || currentProof != nil && !oldProof && !newProof {
		return fmt.Errorf("%w: distributed renewal transaction conflicts with current state", ErrRunConflict)
	}
	if !newProof {
		if !oldProof {
			return fmt.Errorf("%w: distributed renewal proof changed during recovery", ErrRunConflict)
		}
		if err := writePortDistributedProof(s, &txn.Proof); err != nil {
			return err
		}
	}
	if !after {
		if !before {
			return fmt.Errorf("%w: distributed renewal activation changed during recovery", ErrRunConflict)
		}
		if err := writePortActivation(s, &txn.Activation); err != nil {
			return err
		}
	}
	if err := os.Remove(s.portDistributedRenewalTxnPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsyncDir(s.root)
}

// RenewPortActivationWithProof durably publishes a refreshed proof and its
// activation freshness fields as one recoverable filesystem transition.
func (s *FilesystemRunStore) RenewPortActivationWithProof(ctx context.Context, renewal PortActivationRenewal,
	proof *PortDistributedProof) (*PortActivation, error) {
	now := time.Now().UTC()
	if proof == nil {
		return nil, fmt.Errorf("%w: distributed renewal is missing its proof", ErrPortActivation)
	}
	canonical, err := canonicalPortSnapshot(proof.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid distributed proof snapshot", ErrPortActivation)
	}
	normalizedProof := *proof
	normalizedProof.Snapshot = canonical
	proof = &normalizedProof
	activation, proofLock, err := s.distributedActivationLocks()
	if err != nil {
		return nil, err
	}
	defer func() { _ = activation.Unlock() }()
	defer func() { _ = proofLock.Unlock() }()
	if err := s.recoverDistributedStateLocked(ctx); err != nil {
		return nil, err
	}
	current, err := s.loadPortActivationUnlocked(ctx)
	if err != nil {
		return nil, err
	}
	currentProof, err := s.loadPortDistributedProofUnlocked(ctx)
	if err != nil {
		return nil, err
	}
	if current == nil || current.Scope != PortActivationDistributed || !current.Enabled ||
		current.Revision != renewal.PolicyRevision || current.ProofRevision != renewal.ProofRevision ||
		current.ProofDigest != renewal.ProofDigest || current.RefreshLease == nil ||
		!sameRefreshLease(current.RefreshLease, renewal.Lease) || !now.Before(current.ExpiresAt) ||
		current.VerifiedAt.After(renewal.VerifiedAt) || !proofMatchesRenewal(currentProof, renewal, current.StoreIdentity, renewal.ProofRevision) {
		return nil, fmt.Errorf("%w: activation policy, proof or authority lease changed", ErrRunConflict)
	}
	next := *current
	next.ProofRevision = renewal.ProofRevision + 1
	next.VerifiedAt = renewal.VerifiedAt
	next.ExpiresAt = renewal.ExpiresAt
	if err := ValidatePortDistributedRenewalWrite(renewal, proof, &next, now); err != nil {
		return nil, err
	}
	txn := portDistributedRenewalTxn{Version: 1, Renewal: renewal, Proof: *proof, Activation: next}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(txn); err != nil {
		return nil, err
	}
	if err := WriteFileAtomic(s.portDistributedRenewalTxnPath(), bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}), filePerm); err != nil {
		return nil, err
	}
	if err := s.recoverDistributedStateLocked(ctx); err != nil {
		return nil, err
	}
	return &next, nil
}
