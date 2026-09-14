package authority

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// StoredProofVerifier is the launch-time verifier used by compatible server,
// runner and CLI processes. It only reads the authority snapshot persisted by
// the trusted server path; it has no Kubernetes or NATS system credentials.
// A raw backend without this explicitly installed capability therefore cannot
// turn an activation record or an evidence string into admission.
type StoredProofVerifier struct {
	Proofs        store.PortDistributedProofStore
	StoreIdentity string
}

func (v StoredProofVerifier) VerifyPortDistributedActivation(ctx context.Context, record *store.PortActivation, now time.Time) error {
	if record == nil || record.Scope != store.PortActivationDistributed || v.Proofs == nil || v.StoreIdentity == "" || record.StoreIdentity != v.StoreIdentity {
		return fmt.Errorf("stored distributed proof verifier is not configured for this store")
	}
	proof, err := v.Proofs.LoadPortDistributedProof(ctx)
	if err != nil || proof == nil || proof.Validate() != nil || proof.PolicyRevision != record.Revision ||
		proof.ProofRevision != record.ProofRevision || proof.StoreIdentity != record.StoreIdentity ||
		proof.ProofDigest != record.ProofDigest || !now.Before(proof.ExpiresAt) || now.Before(proof.VerifiedAt) {
		return fmt.Errorf("stored distributed proof is absent, stale or does not match activation")
	}
	var snapshot DeploymentCorroboration
	if err := json.Unmarshal(proof.Snapshot, &snapshot); err != nil ||
		snapshot.Epoch != proof.AuthorityEpoch || snapshot.ObservationDigest != proof.ObservationDigest {
		return fmt.Errorf("stored distributed proof snapshot has an inconsistent envelope")
	}
	if snapshot.Queue.Validate() != nil || snapshot.ObservationDigest != record.ProofDigest {
		return fmt.Errorf("stored distributed proof snapshot has an inconsistent queue or digest")
	}
	return nil
}
