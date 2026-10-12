package platformcfg

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// RoutingPolicyRecord is one TENANT level's adaptive-routing policy
// (ADR-121): an org's or a team's override block, one document per
// tenant in its own collection — not platform_settings (that
// collection's documents are platform-scoped, one per family) and not
// the identity directory rows (whose whole-row replaces and three
// independent editors make a bad home for governed policy). The zero
// UpdatedAt of an absent document is the first-write CAS token.
type RoutingPolicyRecord struct {
	Policy    *llmroute.Policy `bson:"policy"      json:"policy"`
	UpdatedAt time.Time        `bson:"updated_at"  json:"updated_at"`
	UpdatedBy string           `bson:"updated_by"  json:"updated_by"`
}

// The document ids of the two tenant levels the launch-time fold reads.
func OrgRoutingPolicyID(orgID string) string { return "org:" + orgID }

func TeamRoutingPolicyID(teamID string) string { return "team:" + teamID }
