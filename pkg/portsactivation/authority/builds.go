package authority

import (
	"fmt"
	"slices"
	"strings"
)

// BuildBinding joins a protected NATS principal and every named holder to a
// tested, immutable build approved by the deployment operator. Its fields
// contain no credential or broker configuration source material.
type BuildBinding struct {
	Account          string `json:"account"`
	Principal        string `json:"principal"`
	HolderID         string `json:"holder_id"`
	ImageDigest      string `json:"image_digest"`
	BuildDigest      string `json:"build_digest"`
	CapabilityDigest string `json:"capability_digest"`
}

type BuildCorroboration struct {
	Bindings []BuildBinding `json:"bindings"`
}

// CorroborateProtectedBuilds refuses a named holder whose credential can
// publish, consume, acknowledge, alter or spoof the shared queue when its exact image
// and build lack operator approval. It checks disconnected holders too. The
// operator still must supply an exhaustive custody inventory, and an
// approval must correspond to a genuinely tested immutable binary.
func CorroborateProtectedBuilds(record *Record, static *StaticAnalysis) (*BuildCorroboration, error) {
	if record == nil || record.validate() != nil || static == nil ||
		len(static.Brokers) != len(record.Brokers) ||
		len(static.Access) != len(record.Credentials)*len(record.Brokers) {
		return nil, fmt.Errorf("distributed build custody requires complete static authority")
	}
	approved := make(map[string]BuildApproval, len(record.BuildApprovals))
	for _, approval := range record.BuildApprovals {
		approved[approval.ImageDigest] = approval
	}
	holders := make(map[string]CredentialHolder, len(record.Holders))
	for _, holder := range record.Holders {
		holders[holder.ID] = holder
	}
	access := make(map[[3]string]StaticAccess, len(static.Access))
	for _, principal := range static.Access {
		key := [3]string{principal.ServerID, principal.Account, principal.Identity}
		if _, exists := access[key]; exists {
			return nil, fmt.Errorf("distributed build custody has duplicate broker principal evidence")
		}
		access[key] = principal
	}
	result := &BuildCorroboration{}
	for _, credential := range record.Credentials {
		if credential.Account != record.Queue.Account {
			continue
		}
		var protected bool
		for index, broker := range record.Brokers {
			principal, exists := access[[3]string{broker.ServerID, credential.Account, credential.Identity}]
			if !exists {
				return nil, fmt.Errorf("distributed build custody is missing a broker principal")
			}
			requires, err := requiresCompatibleBuild(principal)
			if err != nil || index > 0 && requires != protected {
				return nil, fmt.Errorf("distributed build custody has inconsistent protected access")
			}
			protected = requires
		}
		if !protected {
			continue
		}
		for _, holderID := range credential.HolderIDs {
			holder := holders[holderID]
			approval, exists := approved[holder.ImageDigest]
			if !exists || approval.BuildDigest != holder.BuildDigest {
				return nil, fmt.Errorf("distributed queue access remains with an unapproved build")
			}
			result.Bindings = append(result.Bindings, BuildBinding{Account: credential.Account,
				Principal: credential.Identity, HolderID: holderID, ImageDigest: approval.ImageDigest,
				BuildDigest: approval.BuildDigest, CapabilityDigest: approval.CapabilityDigest})
		}
	}
	slices.SortFunc(result.Bindings, func(a, b BuildBinding) int {
		return strings.Compare(a.Account+"\x00"+a.Principal+"\x00"+a.HolderID,
			b.Account+"\x00"+b.Principal+"\x00"+b.HolderID)
	})
	return result, nil
}

func requiresCompatibleBuild(access StaticAccess) (bool, error) {
	protected := false
	for _, exposure := range access.Exposures {
		switch exposure.Surface {
		case "jetstream_api", "acknowledgment", "jetstream_reply_injection":
			if exposure.Direction == "publish" {
				protected = true
			}
		case "queue_messages":
			if exposure.Direction == "publish" || exposure.Direction == "subscribe" {
				protected = true
			}
		case "native_control", "queue_kv":
			if exposure.Direction == "publish" {
				protected = true
			}
		case "jetstream_request_visibility", "jetstream_reply":
			// Read-only visibility alone does not fetch or alter the durable.
		case "unreviewed_jetstream_api", "system_authority":
			return false, fmt.Errorf("unsupported protected NATS authority surface")
		default:
			return false, fmt.Errorf("unclassified protected NATS authority surface")
		}
	}
	return protected, nil
}
