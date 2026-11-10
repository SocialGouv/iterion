package cloudpublisher

import (
	"context"
	"slices"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gatewayRoutes is what gatewayRouting answers about one run.
type gatewayRoutes struct {
	// view is the view every later derivation of the run reads — the
	// gateway routes attributed to the key when one funds them.
	view model.RouteView
	// envFunded: every route rides the gateway and nothing of a tier funds
	// it — the walk acquires nothing.
	envFunded bool
	// sealable: the run may ride a gateway route, so a tier may seal the
	// gateway slot for it (sharedTierPolicy.withGatewaySlot): the walk
	// attributes a gateway route to the key, or some route is one the walk
	// cannot decide — not narrow-safe, or deferring to the runner's
	// environment (a `${DIAL:-vendor/…}` the runner may expand to the
	// gateway). A decided vendor-only run never carries the pair: on the
	// runner it pins the gateway host on the run's proxy, makes the run
	// always-proxied and keeps a subbot out of its parent's sandbox, all for
	// a key no route spends.
	sealable bool
}

// gatewayRouting is ADR-131 §5's one answer about a run's gateway routes,
// shared by the credential walk and the credential preview (the parity
// doctrine: a preview reading its own predicate would show a slot the launch
// refuses, or hide one it seals). It returns the view every later derivation
// of the run reads, whether the run is env-funded on that view, and whether
// the gateway slot may seal for it.
//
// The run's DECIDED gateway routes (a literal `openai_compatible/…` model)
// are attributed to the gateway key — model.RouteView.GatewayKeyFunded — when
// a credential tier the walk consults holds a gateway key serving this launch
// (gatewayKeyHeld), or when the team's sovereign posture (llm_fallback=none)
// owes them one outside a runner pool: such a team's gateway route that
// nothing of its own funds is refused by name, never served by the pod's
// environment. A pool-stamped run without a key keeps the env-funded reading
// — its pod's gateway is the team's own. Otherwise the view is returned
// unchanged and the run is env-funded exactly when every route rides the
// gateway, as before.
//
// The stores are read only when the attributed reading names a gateway route
// at all, so a run that never routes to the gateway costs nothing here.
// sovereign and pooled are the walk's postures (llm_fallback=none, a runner
// pool mapping); either keeps the shared tiers out of the probe, as out of
// the walk. A nil workflow is a program nobody read: it fails open, gateway
// slot included.
func (p *Publisher) gatewayRouting(ctx context.Context, view model.RouteView, tenantID, orgID, ownerID, audienceBotID string, sovereign, pooled bool) gatewayRoutes {
	if view.Workflow == nil {
		return gatewayRoutes{view: view, sealable: true}
	}
	gw := string(secrets.ProviderOpenAICompatible)
	keyed := view
	keyed.GatewayKeyFunded = true
	walk := model.EffectiveProviders(keyed, walkProviderSet)
	attributed := slices.Contains(walk.Providers, gw) &&
		((sovereign && !pooled) || p.gatewayKeyHeld(ctx, tenantID, orgID, ownerID, audienceBotID, sovereign || pooled))
	if !attributed {
		keyed, walk = view, model.EffectiveProviders(view, walkProviderSet)
	}
	return gatewayRoutes{
		view:      keyed,
		envFunded: walk.OnlyEnvFunded(),
		sealable:  slices.Contains(walk.Providers, gw) || !walk.NarrowSafe || walk.EnvDeferred,
	}
}

// gatewayKeyHeld answers "the team holds a gateway key" (ADR-131 §5): whether
// a credential tier this launch's walk consults holds an openai_compatible
// key serving the launch. The tiers are the walk's: the team's own (its keys
// and the owner's personal ones), then — unless sharedTiersBlocked (a
// sovereign or pool-stamped team, D13) — the org tier its audience admits the
// team to and the platform tier its audience admits. A key serves the launch
// under the SAME selection and audiences secrets.Resolve applies
// (OrderedAPIKeyCandidates, ServesBotForLaunch, ServesRepoForLaunch), and
// WITHOUT the usability predicate: a key at its ceiling, refused or over
// budget still makes the routes the team's to fund — the walk's restore hands
// it back, and the run parks on it rather than spending the pod's gateway.
//
// A tier whose keys cannot be listed is read as HOLDING one, said out loud:
// the walk then consults that tier, whose own read decides — the tenant's
// fails the launch, a shared tier's skips with its own line — where reading
// it as empty would send the team's routes to the pod's money in silence.
func (p *Publisher) gatewayKeyHeld(ctx context.Context, tenantID, orgID, ownerID, audienceBotID string, sharedTiersBlocked bool) bool {
	if p.apiKeys == nil || tenantID == "" {
		return false
	}
	repo := secrets.LaunchRepoFromContext(ctx)
	holds := func(tier, scope, userID string) bool {
		visible, err := p.apiKeys.ListByTeam(store.WithTenant(ctx, scope), scope, userID)
		if err != nil {
			if p.logger != nil {
				p.logger.Warn("cloudpublisher: %s tier key list for the gateway probe: %v — read as holding a gateway key: the walk consults the tier and its own read decides", tier, err)
			}
			return true
		}
		for _, c := range secrets.OrderedAPIKeyCandidates(visible, userID, []secrets.Provider{secrets.ProviderOpenAICompatible}, nil) {
			if c.Key.ServesBotForLaunch(audienceBotID) && c.Key.ServesRepoForLaunch(repo) {
				return true
			}
		}
		return false
	}
	if holds("tenant", tenantID, ownerID) {
		return true
	}
	if sharedTiersBlocked {
		return false
	}
	if orgID != "" && p.orgCredentialAudience(ctx, orgID, tenantID) && holds("org", secrets.OrgTierTenantID(orgID), "") {
		return true
	}
	if p.platformAudience != nil && !p.platformAudience.Get(ctx).Allows(orgID, tenantID) {
		return false
	}
	return holds("platform", secrets.PlatformTenantID, "")
}
