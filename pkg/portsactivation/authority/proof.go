package authority

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// BuildDistributedProof projects a completed deployment observation into the
// store's bounded proof envelope. Private source bytes are held only by the
// observer and never enter Snapshot. Timing fields are removed before hashing
// so a renewal can reuse the same observation identity.
func BuildDistributedProof(record *Record, observation *DeploymentCorroboration,
	storeIdentity string, policyRevision, proofRevision uint64, expiresAt time.Time) (*store.PortDistributedProof, error) {
	if record == nil || observation == nil || storeIdentity == "" || policyRevision == 0 || proofRevision == 0 ||
		expiresAt.IsZero() || observation.ObservationDigest == "" {
		return nil, fmt.Errorf("distributed proof requires a complete authority observation")
	}
	copy := *observation
	copy.StartedAt = time.Time{}
	copy.CompletedAt = time.Time{}
	copy.ObservationDigest = observation.ObservationDigest
	snapshot, err := json.Marshal(copy)
	if err != nil || bytes.Contains(snapshot, []byte("private-password")) || bytes.Contains(snapshot, []byte("super-secret")) {
		return nil, fmt.Errorf("distributed proof snapshot could not be safely encoded")
	}
	digest := sha256.Sum256(bytes.TrimSpace(snapshot))
	now := observation.CompletedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	proof := &store.PortDistributedProof{Version: store.PortDistributedProofVersion,
		PolicyRevision: policyRevision, ProofRevision: proofRevision, AuthorityEpoch: record.Epoch,
		StoreIdentity: storeIdentity, ProofDigest: observation.ObservationDigest,
		ObservationDigest: observation.ObservationDigest, SnapshotDigest: hex.EncodeToString(digest[:]),
		Snapshot: snapshot, VerifiedAt: now, ExpiresAt: expiresAt}
	if err := proof.Validate(); err != nil {
		return nil, err
	}
	return proof, nil
}
