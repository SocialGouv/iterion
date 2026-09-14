package authority

import (
	"fmt"
	"slices"
	"strings"
	"time"

	queue "github.com/SocialGouv/iterion/pkg/queue/nats"
)

// CensusMember names an active announcement without credential material. A
// broker-assigned KV timestamp and revision make stale or overwritten claims
// visible, but the claim itself is still made by a connected process.
type CensusMember struct {
	Principal  string    `json:"principal"`
	Instance   string    `json:"instance"`
	HolderID   string    `json:"holder_id"`
	Revision   uint64    `json:"revision"`
	RecordedAt time.Time `json:"recorded_at"`
}

// CensusCorroboration covers the instances seen in one complete KV snapshot.
// It neither proves that disconnected holders are gone nor that each NATS
// principal can write only its own census key. The effective ACL and trusted
// operator custody inventory must establish those properties separately.
type CensusCorroboration struct {
	Members []CensusMember `json:"members"`
}

// CorroborateObservedCensus rejects stale, unknown or incompatible claims in
// the complete snapshot returned by Conn.PortCapabilities. The server supplies
// its own backend and runner-epoch values; the operator's immutable-build
// approvals bind each observed capability. No value from a tenant
// request can substitute for them. The result is not an activation proof.
func CorroborateObservedCensus(record *Record, observations []queue.PortCapabilityObservation,
	storeIdentity string, runnerEpoch uint64, now time.Time) (*CensusCorroboration, error) {
	if record == nil || record.validate() != nil || storeIdentity == "" ||
		now.IsZero() || len(observations) == 0 || len(observations) > 10000 {
		return nil, fmt.Errorf("distributed capability census has an invalid authority scope")
	}
	holders := make(map[string]CredentialHolder, len(record.Holders))
	for _, holder := range record.Holders {
		holders[holder.ID] = holder
	}
	approved := make(map[string]BuildApproval, len(record.BuildApprovals))
	for _, approval := range record.BuildApprovals {
		approved[approval.ImageDigest] = approval
	}
	credentials := make(map[string]CredentialCustody)
	for _, credential := range record.Credentials {
		if credential.Account != record.Queue.Account {
			continue
		}
		if _, err := queue.PortCensusKey(credential.Identity, "instance"); err != nil {
			return nil, fmt.Errorf("distributed capability census has an unsupported queue principal")
		}
		credentials[credential.Identity] = credential
	}
	seen := make(map[string]bool, len(observations))
	result := &CensusCorroboration{Members: make([]CensusMember, 0, len(observations))}
	for _, observed := range observations {
		claim := observed.Capability
		key, err := queue.PortCensusKey(claim.Principal, claim.Instance)
		if err != nil || seen[key] || observed.Revision == 0 || claim.Validate() != nil || !observed.Fresh(now) ||
			claim.Account != record.Queue.Account || claim.Stream != record.Queue.Stream ||
			claim.Consumer != record.Queue.Consumer || claim.StoreIdentity != storeIdentity ||
			claim.RunnerEpoch != runnerEpoch ||
			claim.AuthorityEpoch != record.Epoch {
			return nil, fmt.Errorf("distributed capability census has a stale or contradictory instance")
		}
		seen[key] = true
		credential, exists := credentials[claim.Principal]
		if !exists {
			return nil, fmt.Errorf("distributed capability census has an unaccounted principal")
		}
		matched := ""
		for _, holderID := range credential.HolderIDs {
			holder := holders[holderID]
			if "sha256:"+holder.BuildDigest != claim.BuildDigest {
				continue
			}
			if matched != "" {
				return nil, fmt.Errorf("distributed capability census cannot identify a unique credential holder")
			}
			matched = holderID
		}
		if matched == "" {
			return nil, fmt.Errorf("distributed capability census build is outside trusted custody")
		}
		holder := holders[matched]
		approval, okay := approved[holder.ImageDigest]
		if !okay || approval.BuildDigest != holder.BuildDigest ||
			approval.CapabilityDigest != claim.CapabilityDigest {
			return nil, fmt.Errorf("distributed capability census build is outside tested approval")
		}
		result.Members = append(result.Members, CensusMember{Principal: claim.Principal,
			Instance: claim.Instance, HolderID: matched, Revision: observed.Revision,
			RecordedAt: observed.RecordedAt})
	}
	slices.SortFunc(result.Members, func(a, b CensusMember) int {
		return strings.Compare(a.Principal+"\x00"+a.Instance, b.Principal+"\x00"+b.Instance)
	})
	return result, nil
}
