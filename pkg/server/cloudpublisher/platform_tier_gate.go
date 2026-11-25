package cloudpublisher

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// blocksSharedTiers is the ONE expression for the two team postures that
// keep every shared tier (org, pool, platform) out of a launch: a team mapped
// to a runner pool (D13 — a pool pod would hold other tenants' keys) and a
// sovereign team (llm_fallback=none — the platform lends it nothing). The
// tier walk, the fallback door, the gateway probe, the credential preview and
// the model catalog's platform gate (PlatformTierConsulted) all read it.
func blocksSharedTiers(pooled, sovereign bool) bool {
	return pooled || sovereign
}

// platformAudienceAdmits answers the platform credential audience for
// (orgID, tenantID), without logging: no resolver wired admits everyone, as
// do an absent and an unenforced record (PlatformCredentials.Allows); a
// store failure serves the last-known record (Resolver.Get).
// platformAudienceAllows is the launch's logged reading of it.
func (p *Publisher) platformAudienceAdmits(ctx context.Context, orgID, tenantID string) bool {
	return p.platformAudience == nil || p.platformAudience.Get(ctx).Allows(orgID, tenantID)
}

// PlatformTierConsulted reports whether a TRUSTED launch of teamID, as the
// stores read now, consults the platform credential tier: the team is
// neither mapped to a runner pool nor sovereign (blocksSharedTiers), and the
// platform credential audience admits it with the org the launch charges
// (orgIDForTeam). The model catalog reads it before counting platform
// credentials as a team's, so a model only the platform funds is never
// shown as usable to a team whose launch would never receive that
// credential.
//
// The pool and policy reads are the launch's own fresh readers, and their
// errors are returned as the launch would refuse on them: an unknown answer
// is never read as "consulted". An empty teamID consults nothing. A refusal
// is not logged — it is the answer, not an incident.
func (p *Publisher) PlatformTierConsulted(ctx context.Context, teamID string) (bool, error) {
	if teamID == "" {
		return false, nil
	}
	pool, err := p.runnerPoolForTeam(ctx, teamID)
	if err != nil {
		return false, err
	}
	policy, _, err := p.llmFallbackForTeam(ctx, teamID)
	if err != nil {
		return false, err
	}
	if blocksSharedTiers(pool != "", policy == identity.LLMFallbackNone) {
		return false, nil
	}
	return p.platformAudienceAdmits(ctx, p.orgIDForTeam(ctx, teamID), teamID), nil
}
