package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const PortDistributedProofVersion = 1

// PortDistributedProof is the trusted server's safe projection of one
// privileged deployment observation. Snapshot contains no credential-bearing
// URLs or NATS/Kubernetes source bytes; its digest binds the exact JSON.
type PortDistributedProof struct {
	Version           int             `json:"version" bson:"version"`
	PolicyRevision    uint64          `json:"policy_revision" bson:"policy_revision"`
	ProofRevision     uint64          `json:"proof_revision" bson:"proof_revision"`
	AuthorityEpoch    uint64          `json:"authority_epoch" bson:"authority_epoch"`
	StoreIdentity     string          `json:"store_identity" bson:"store_identity"`
	ProofDigest       string          `json:"proof_digest" bson:"proof_digest"`
	ObservationDigest string          `json:"observation_digest" bson:"observation_digest"`
	SnapshotDigest    string          `json:"snapshot_digest" bson:"snapshot_digest"`
	Snapshot          json.RawMessage `json:"snapshot" bson:"snapshot"`
	VerifiedAt        time.Time       `json:"verified_at" bson:"verified_at"`
	ExpiresAt         time.Time       `json:"expires_at" bson:"expires_at"`
}

func (p *PortDistributedProof) Validate() error {
	if p == nil || p.Version != PortDistributedProofVersion || p.PolicyRevision == 0 || p.PolicyRevision > math.MaxInt64 || p.ProofRevision == 0 || p.ProofRevision > math.MaxInt64 ||
		p.AuthorityEpoch == 0 || p.StoreIdentity == "" || !isPortDigest(p.ProofDigest) ||
		!isPortDigest(p.ObservationDigest) || !isPortDigest(p.SnapshotDigest) || len(p.Snapshot) == 0 ||
		len(p.Snapshot) > 1<<20 || !json.Valid(p.Snapshot) || p.VerifiedAt.IsZero() ||
		!p.ExpiresAt.After(p.VerifiedAt) || p.ExpiresAt.After(p.VerifiedAt.Add(PortDistributedProofMaxAge)) {
		return fmt.Errorf("%w: malformed distributed proof snapshot", ErrPortActivation)
	}
	canonical, err := canonicalPortSnapshot(p.Snapshot)
	if err != nil {
		return fmt.Errorf("%w: invalid distributed proof snapshot", ErrPortActivation)
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != p.SnapshotDigest {
		return fmt.Errorf("%w: distributed proof snapshot digest mismatch", ErrPortActivation)
	}
	return nil
}

func canonicalPortSnapshot(raw json.RawMessage) (json.RawMessage, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	return bytes.Clone(compact.Bytes()), nil
}

func isPortDigest(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

// PortActivationRefreshLease carries the current NATS KV lease revision into
// the activation document's CAS boundary. A refresher must renew its existing
// NATS lease before claiming this fence, then finish before ExpiresAt. The
// deadline starts before the NATS request, not when its response arrives.
// Claiming a fence does not renew the authorization proof or enable admission.
type PortActivationRefreshLease struct {
	Owner     string    `json:"owner" bson:"owner"`
	Token     uint64    `json:"token" bson:"token"`
	ExpiresAt time.Time `json:"expires_at" bson:"expires_at"`
}

func (l *PortActivationRefreshLease) Validate() error {
	if l == nil || l.Owner == "" || l.Token == 0 || l.Token > math.MaxInt64 || l.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: malformed authority refresh lease", ErrPortActivation)
	}
	return nil
}

// PortActivationRenewal can update freshness only. It deliberately carries no
// Enabled flag, new policy, scope, capability or authority fingerprint. The
// privileged observer must compare its verified fingerprint with ProofDigest
// before invoking this operation; an observation is never caller supplied.
type PortActivationRenewal struct {
	PolicyRevision uint64                     `json:"policy_revision"`
	ProofRevision  uint64                     `json:"proof_revision"`
	ProofDigest    string                     `json:"proof_digest"`
	Lease          PortActivationRefreshLease `json:"lease"`
	VerifiedAt     time.Time                  `json:"verified_at"`
	ExpiresAt      time.Time                  `json:"expires_at"`
}

func (r PortActivationRenewal) Validate(now time.Time) error {
	if r.PolicyRevision == 0 || r.PolicyRevision > math.MaxInt64 || r.ProofRevision == 0 || r.ProofRevision >= math.MaxInt64 ||
		len(r.ProofDigest) != 64 || strings.Trim(r.ProofDigest, "0123456789abcdef") != "" ||
		r.Lease.Validate() != nil || !now.Before(r.Lease.ExpiresAt) ||
		r.VerifiedAt.IsZero() || r.VerifiedAt.After(now) || !now.Before(r.ExpiresAt) ||
		!r.ExpiresAt.After(r.VerifiedAt) || r.ExpiresAt.After(r.VerifiedAt.Add(PortDistributedProofMaxAge)) {
		return fmt.Errorf("%w: invalid or expired authority renewal", ErrPortActivation)
	}
	return nil
}

// ValidatePortDistributedRenewalWrite checks the proof and activation payload
// that a backend is about to publish together. The renewal itself identifies
// the pre-write activation; the proof must be the next revision and carry the
// exact freshness interval observed by the authority.
func ValidatePortDistributedRenewalWrite(renewal PortActivationRenewal, proof *PortDistributedProof, next *PortActivation, now time.Time) error {
	if err := renewal.Validate(now); err != nil {
		return err
	}
	if proof == nil || next == nil {
		return fmt.Errorf("%w: distributed renewal is missing its proof or activation", ErrPortActivation)
	}
	if err := proof.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if proof.PolicyRevision != renewal.PolicyRevision || proof.ProofRevision != renewal.ProofRevision+1 ||
		proof.ProofDigest != renewal.ProofDigest || proof.StoreIdentity != next.StoreIdentity ||
		!proof.VerifiedAt.Equal(renewal.VerifiedAt) ||
		!proof.ExpiresAt.Equal(renewal.ExpiresAt) || next.Revision != renewal.PolicyRevision ||
		next.ProofRevision != proof.ProofRevision || next.ProofDigest != proof.ProofDigest ||
		next.Scope != PortActivationDistributed || !next.Enabled || next.RefreshLease == nil ||
		next.RefreshLease.Owner != renewal.Lease.Owner || next.RefreshLease.Token != renewal.Lease.Token ||
		!next.RefreshLease.ExpiresAt.Equal(renewal.Lease.ExpiresAt) || !next.VerifiedAt.Equal(renewal.VerifiedAt) ||
		!next.ExpiresAt.Equal(renewal.ExpiresAt) {
		return fmt.Errorf("%w: distributed proof and renewal do not describe one transition", ErrPortActivation)
	}
	return nil
}

// PortActivationRefreshStore is the distributed authority's narrow write
// surface. The Store and all holders of its write credentials are trusted;
// these CAS operations do not authenticate database credential holders.
// Production handlers must restrict access to the operator authority path.
type PortActivationRefreshStore interface {
	// ClaimPortActivationRefresh installs a newer NATS fencing token for an
	// already enabled policy. Its atomic result supplies the proof revision
	// to renew. It must neither change proof freshness nor supersede Disable.
	ClaimPortActivationRefresh(context.Context, uint64, PortActivationRefreshLease) (*PortActivation, error)
	RenewPortActivation(context.Context, PortActivationRenewal) (*PortActivation, error)
}

// PortDistributedRenewalWriter atomically publishes a refreshed proof and the
// activation freshness fields that reference it. Production authority paths
// must use this interface; the lower-level RenewPortActivation method remains
// for storage/CAS conformance tests and cannot by itself publish a proof.
type PortDistributedRenewalWriter interface {
	RenewPortActivationWithProof(context.Context, PortActivationRenewal, *PortDistributedProof) (*PortActivation, error)
}

// PortActivationDisabler changes operator policy independently of ongoing
// freshness revisions. Implementations leave the latest observations intact.
type PortActivationDisabler interface {
	DisablePortActivation(context.Context, uint64) (*PortActivation, error)
}

// PortDistributedProofStore is the backend for an immutable authority
// snapshot. Only the trusted server path may write it.
type PortDistributedProofStore interface {
	LoadPortDistributedProof(context.Context) (*PortDistributedProof, error)
	SavePortDistributedProof(context.Context, uint64, *PortDistributedProof) error
}

// PortDistributedActivationWriter publishes a new distributed proof and the
// activation that references it as one fenced state transition. Implementors
// must leave both the previous proof and previous activation untouched when
// the expected policy revision no longer matches. This is deliberately
// separate from PortActivationStore: ordinary activation writes do not carry
// enough information to publish a proof safely.
type PortDistributedActivationWriter interface {
	SavePortDistributedActivation(context.Context, uint64, *PortDistributedProof, *PortActivation) error
}

// PortDistributedProofCandidateStore holds the latest probe result separately
// from the proof currently referenced by an enabled activation. Probing can
// therefore be repeated without temporarily invalidating live admission.
type PortDistributedProofCandidateStore interface {
	LoadPortDistributedProofCandidate(context.Context) (*PortDistributedProof, error)
	SavePortDistributedProofCandidate(context.Context, *PortDistributedProof) error
	ClearPortDistributedProofCandidate(context.Context) error
}
