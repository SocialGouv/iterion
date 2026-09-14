package authority

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// RefreshLeaseSource supplies the monotonic NATS/KV fencing token owned by
// this server instance. Implementations must obtain the token before calling
// ClaimPortActivationRefresh and must not share it with ordinary runners.
type RefreshLeaseSource interface {
	Acquire(context.Context, uint64, time.Time) (store.PortActivationRefreshLease, error)
}

// Refresher renews an already-enabled distributed activation. It never
// creates an activation and never changes the operator policy revision.
type Refresher struct {
	Authority *Authority
	Leases    RefreshLeaseSource
	Interval  time.Duration
	Now       func() time.Time
}

func (r *Refresher) Run(ctx context.Context) error {
	if r == nil || r.Authority == nil || r.Authority.Adapter == nil || r.Authority.Activations == nil ||
		r.Authority.Proofs == nil || r.Leases == nil {
		return fmt.Errorf("distributed authority refresher is not configured")
	}
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	now := r.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.refresh(ctx, now()); err != nil {
				// A failed observation must not disable a still-valid proof
				// early; expiration remains the hard upper bound. The next
				// tick retries under a fresh lease.
				continue
			}
		}
	}
}

func (r *Refresher) refresh(ctx context.Context, now time.Time) error {
	current, err := r.Authority.Activations.LoadPortActivation(ctx)
	if err != nil {
		return err
	}
	if current == nil || !current.Enabled || current.Scope != store.PortActivationDistributed ||
		current.Revision == 0 || current.ProofRevision >= math.MaxInt64-1 {
		return nil
	}
	refreshStore, ok := r.Authority.Activations.(store.PortActivationRefreshStore)
	if !ok {
		return fmt.Errorf("distributed authority store has no refresh CAS")
	}
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	lease, err := r.Leases.Acquire(ctx, current.Revision, now.Add(interval*2))
	if err != nil {
		return err
	}
	claimed, err := refreshStore.ClaimPortActivationRefresh(ctx, current.Revision, lease)
	if err != nil {
		return err
	}
	if claimed == nil || claimed.RefreshLease == nil {
		return fmt.Errorf("distributed authority store returned no claimed refresh lease")
	}
	// Mongo stores dates at millisecond precision. Use the lease value returned
	// by the CAS write rather than the pre-write NATS value so the subsequent
	// exact ownership predicate compares the backend's representation.
	claimedLease := *claimed.RefreshLease
	record, err := r.Authority.Adapter.readRecord(ctx)
	if err != nil {
		return err
	}
	proof, err := r.Authority.Adapter.ProbeForRecord(ctx, record, r.Authority.StoreIdentity, current.Revision, claimed.ProofRevision+1,
		now.Add(store.PortDistributedProofMaxAge))
	if err != nil {
		return err
	}
	if proof.ProofDigest != current.ProofDigest {
		return fmt.Errorf("distributed authority fingerprint changed; explicit reactivation required")
	}
	renewal := store.PortActivationRenewal{
		PolicyRevision: current.Revision, ProofRevision: claimed.ProofRevision,
		ProofDigest: proof.ProofDigest, Lease: claimedLease, VerifiedAt: proof.VerifiedAt, ExpiresAt: proof.ExpiresAt}
	writer, ok := refreshStore.(store.PortDistributedRenewalWriter)
	if !ok {
		return fmt.Errorf("distributed authority store cannot atomically publish a refreshed proof")
	}
	_, err = writer.RenewPortActivationWithProof(ctx, renewal, proof)
	return err
}
