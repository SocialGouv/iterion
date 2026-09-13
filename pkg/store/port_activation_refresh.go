package store

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

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
