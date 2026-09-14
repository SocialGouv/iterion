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
	if p == nil || p.Version != PortDistributedProofVersion || p.PolicyRevision == 0 || p.ProofRevision == 0 ||
		p.AuthorityEpoch == 0 || p.StoreIdentity == "" || !isPortDigest(p.ProofDigest) ||
		!isPortDigest(p.ObservationDigest) || !isPortDigest(p.SnapshotDigest) || len(p.Snapshot) == 0 ||
		len(p.Snapshot) > 1<<20 || !json.Valid(p.Snapshot) || p.VerifiedAt.IsZero() ||
		!p.ExpiresAt.After(p.VerifiedAt) || p.ExpiresAt.After(p.VerifiedAt.Add(PortDistributedProofMaxAge)) {
		return fmt.Errorf("%w: malformed distributed proof snapshot", ErrPortActivation)
	}
	sum := sha256.Sum256(bytes.TrimSpace(p.Snapshot))
	if hex.EncodeToString(sum[:]) != p.SnapshotDigest {
		return fmt.Errorf("%w: distributed proof snapshot digest mismatch", ErrPortActivation)
	}
	return nil
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
	PolicyRevision uint64
	ProofRevision  uint64
	ProofDigest    string
	Lease          PortActivationRefreshLease
	VerifiedAt     time.Time
	ExpiresAt      time.Time
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
