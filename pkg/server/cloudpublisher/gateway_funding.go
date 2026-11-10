package cloudpublisher

import (
	"context"
	"fmt"
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
	// sealable: a tier may seal the gateway slot for the run
	// (sharedTierPolicy.withGatewaySlot) — the operator's arbitration of
	// #2005 PR-C' round 1, in two arms. The run ATTRIBUTES a gateway route
	// to a key (a decided `openai_compatible/…` route under the keyed
	// view); or some route is one the walk cannot decide — not narrow-safe,
	// or deferring to the runner's environment (a `${DIAL:-vendor/…}` the
	// runner may expand to the gateway) — AND the launch is mapped to a
	// dedicated runner pool (D13), whose pod carries the operator's
	// gateway dials. A default-runner run with undecided routes never
	// carries the pair: its dials do not expand to the gateway, and the
	// pair would start the run's sandbox proxy, pin the gateway host on
	// it, reserve a slot of the key's concurrency cap and open the
	// gateway host in an untrusted run's fork lane, all for a key no
	// route spends. A decided vendor-only run never carries it either.
	sealable bool
	// mayRide: the run may ride a gateway route once a key funds it — a
	// decided gateway route on the keyed reading whatever the probe found,
	// or, under a runner pool, a route the walk cannot decide. It differs
	// from sealable on one shape only: a decided gateway route no tier
	// holds a key for, which the unkeyed reading leaves to the pod's
	// environment. A gateway pin governs every run this names
	// (gatewayPinRefusal): a pinned key the probe cannot see is no reason
	// for the pod's gateway to serve the routes in its place.
	mayRide bool
	// note names what the probe held that cannot fund the run: a gateway key
	// a consulted tier holds for this launch — its tier, name and id — and
	// why it cannot seal (the run's frozen lists hold the slot out, or its
	// sealed secret does not open), or the tier read as holding one because
	// its key list failed. Empty when a key funds the routes, or when no
	// tier holds one. Every statement of the run's gateway fate carries it:
	// the env-funded line, and the unfunded refusal of an attributed route
	// that sealed nothing (heldOut).
	note string
}

// heldOut is the note keyed by the provider the unfunded refusal names: what
// a tier held for it that did not seal. nil when the probe held nothing it
// could not use.
func (g gatewayRoutes) heldOut() map[string]string {
	if g.note == "" {
		return nil
	}
	return map[string]string{string(secrets.ProviderOpenAICompatible): g.note}
}

// gatewayProbe is what the probe reads of the run beyond its identity, so
// it attributes a gateway route only to a key that can seal for the run.
type gatewayProbe struct {
	// lists is the run's seal gate WITHOUT the gateway answer: its frozen
	// routing pair order and providers allowlist, the policy the walk seals
	// through. A held key the lists hold out is noted on the policy's §5
	// record (sealableHeldSlot), which the walk's end says once.
	lists sharedTierPolicy
	// allowed is the run's frozen providers allowlist (nil when none), so
	// the note names which list holds the slot out.
	allowed map[string]bool
	// opens reports why a held key's sealed secret does not open, as the
	// walk opens it (secrets.OpenApiKey); nil assumes it opens — the
	// preview's posture: it opens no secret and assumes materialization.
	opens func(secrets.ApiKey) error
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
// that can seal for the run (gatewayKeyHold: the run's frozen lists admit the
// slot and the key opens), or when the team's sovereign posture
// (llm_fallback=none) owes them one outside a runner pool: such a team's gateway route that
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
func (p *Publisher) gatewayRouting(ctx context.Context, view model.RouteView, tenantID, orgID, ownerID, audienceBotID string, sovereign, pooled bool, probe gatewayProbe) gatewayRoutes {
	if view.Workflow == nil {
		return gatewayRoutes{view: view, sealable: true, mayRide: true}
	}
	gw := string(secrets.ProviderOpenAICompatible)
	keyed := view
	keyed.GatewayKeyFunded = true
	walk := model.EffectiveProviders(keyed, walkProviderSet)
	routed := slices.Contains(walk.Providers, gw)
	// A route the walk cannot decide rides the gateway only where the pod
	// carrying the run expands its dials there: a dedicated runner pool.
	undecidedOnPool := pooled && (!walk.NarrowSafe || walk.EnvDeferred)
	var hold gatewayHold
	if routed {
		hold = p.gatewayKeyHold(ctx, tenantID, orgID, ownerID, audienceBotID, sovereign || pooled, probe)
	}
	attributed := routed && ((sovereign && !pooled) || hold.funds)
	if !attributed {
		keyed, walk = view, model.EffectiveProviders(view, walkProviderSet)
	}
	return gatewayRoutes{
		view:      keyed,
		envFunded: walk.OnlyEnvFunded(),
		sealable:  attributed || undecidedOnPool,
		mayRide:   routed || undecidedOnPool,
		note:      hold.note,
	}
}

// gatewayHold is the probe's answer: whether a consulted tier holds a
// gateway key that funds the run, and the note naming what it held that
// cannot (gatewayRoutes.note).
type gatewayHold struct {
	funds bool
	note  string
}

// gatewayKeyHold answers "the team holds a gateway key that funds the run"
// (ADR-131 §5): whether a credential tier this launch's walk consults holds
// an openai_compatible key serving the launch that can seal for it. The tiers
// are the walk's: the team's own (its keys and the owner's personal ones),
// then — unless sharedTiersBlocked (a sovereign or pool-stamped team, D13) —
// the org tier its audience admits the team to and the platform tier its
// audience admits. A key serves the launch under the SAME selection and
// audiences secrets.Resolve applies (OrderedAPIKeyCandidates,
// ServesBotForLaunch, ServesRepoForLaunch), and WITHOUT the usability
// predicate: a key at its ceiling, refused or over budget still makes the
// routes the team's to fund — the walk's restore hands it back, and the run
// parks on it rather than spending the pod's gateway.
//
// It funds the run only where it can seal: the run's frozen lists admit the
// gateway slot (a key held out by them leaves the routes to the pod's
// gateway, ADR-131 §5's replay clause, and is noted on the §5 record), and
// its sealed secret opens as the walk opens it — the walk's Resolve gives an
// undecryptable key up in silence, the next key of the provider taking its
// place. What was held and could not seal is named in the answer's note.
//
// A tier whose keys cannot be listed is read as HOLDING one, said out loud:
// the walk then consults that tier, whose own read decides — the tenant's
// fails the launch, a shared tier's skips with its own line — where reading
// it as empty would send the team's routes to the pod's money in silence.
func (p *Publisher) gatewayKeyHold(ctx context.Context, tenantID, orgID, ownerID, audienceBotID string, sharedTiersBlocked bool, probe gatewayProbe) gatewayHold {
	if p.apiKeys == nil || tenantID == "" {
		return gatewayHold{}
	}
	repo := secrets.LaunchRepoFromContext(ctx)
	slot := slotOfProvider(secrets.ProviderOpenAICompatible)
	unopened := ""
	// visit answers for one tier: decided reports that the tier settles the
	// probe (a key that funds the run, the lists holding the slot out, or a
	// list that failed); otherwise the walk moves to the next tier, as the
	// walk's own fill would.
	visit := func(tier, scope, userID string) (hold gatewayHold, decided bool) {
		visible, err := p.apiKeys.ListByTeam(store.WithTenant(ctx, scope), scope, userID)
		if err != nil {
			if p.logger != nil {
				p.logger.Warn("cloudpublisher: %s tier key list for the gateway probe: %v — read as holding a gateway key: the walk consults the tier and its own read decides", tier, err)
			}
			return gatewayHold{funds: true, note: fmt.Sprintf("the %s tier's key list could not be read (%v), so the gateway probe read it as holding a gateway key and the walk's own read of that tier decided", tier, err)}, true
		}
		for _, c := range secrets.OrderedAPIKeyCandidates(visible, userID, []secrets.Provider{secrets.ProviderOpenAICompatible}, nil) {
			k := c.Key
			if !k.ServesBotForLaunch(audienceBotID) || !k.ServesRepoForLaunch(repo) {
				continue
			}
			held := fmt.Sprintf("the %s tier's gateway key %q (%s) serves this launch", tier, k.Name, k.ID)
			if !probe.lists.sealableHeldSlot(slot) {
				return gatewayHold{note: held + ", but " + slotHeldOutBy(probe.allowed, slot)}, true
			}
			if probe.opens == nil {
				return gatewayHold{funds: true}, true
			}
			if err := probe.opens(k); err != nil {
				if unopened == "" {
					unopened = held + fmt.Sprintf(", but its sealed secret does not open (%v)", err)
				}
				continue
			}
			return gatewayHold{funds: true}, true
		}
		return gatewayHold{}, false
	}
	if hold, decided := visit("tenant", tenantID, ownerID); decided {
		return hold
	}
	if !sharedTiersBlocked {
		if orgID != "" && p.orgCredentialAudience(ctx, orgID, tenantID) {
			if hold, decided := visit("org", secrets.OrgTierTenantID(orgID), ""); decided {
				return hold
			}
		}
		if p.platformAudience == nil || p.platformAudience.Get(ctx).Allows(orgID, tenantID) {
			if hold, decided := visit("platform", secrets.PlatformTenantID, ""); decided {
				return hold
			}
		}
	}
	return gatewayHold{note: unopened}
}
