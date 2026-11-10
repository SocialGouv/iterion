package cloudpublisher

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
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
	// (sharedTierPolicy.withGatewaySlot) — the operator's arbitration (ADR-131
	// §5's seal rule), in two arms. The run ATTRIBUTES a gateway route
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
	// repoOnly is a gateway key a consulted tier holds that only its
	// repository audience keeps from a launch whose repository is UNKNOWN
	// (the preview's: a personal source, a pattern or several repositories,
	// gatewayProbe.repoUnknown) — a launch on a repository it names seals
	// it, any other rides the runner's gateway. nil on a launch, which
	// always knows its repository.
	repoOnly *heldGatewayKey
	// attributedTo is the key the probe attributed the gateway routes to
	// (gatewayHold.by); nil when none did — the sovereign posture attributing
	// them without one included: those routes are owed a key of the team's
	// own and refused by name, never left to the pod's environment.
	attributedTo *heldGatewayKey
	// replayHeldOut names, on a replay whose frozen stamp records no gateway
	// key the launch sealed, what this resolution would have attributed or
	// sealed and held out instead (gatewayRouting's replay rule). Empty
	// otherwise; the walk warns it.
	replayHeldOut string
}

// heldGatewayKey is a gateway key the probe held, with the tier it lives in;
// poolID names the attached pool when the key is that pool's lent entry.
type heldGatewayKey struct {
	tier   string
	key    secrets.ApiKey
	poolID string
}

// String names the key the way every gateway sentence does: its tier (or its
// attached pool), its name and its id.
func (h heldGatewayKey) String() string {
	if h.poolID != "" {
		return fmt.Sprintf("the attached pool %s's gateway entry %q (%s)", h.poolID, h.key.Name, h.key.ID)
	}
	return fmt.Sprintf("the %s tier's gateway key %q (%s)", h.tier, h.key.Name, h.key.ID)
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
	// repoUnknown marks a probe whose launch repository is not known (the
	// preview's, when it names none): a key its repository audience alone
	// keeps out is reported (gatewayRoutes.repoOnly), never read as absent.
	repoUnknown bool
	// attachment is the launch's resolved pool attachment (ADR-164), nil when
	// none: its pool is a tier the walk consults, so a gateway entry it holds
	// for this launch — its eligibility and its key row's audiences, never its
	// availability (attachedGatewayHold) — attributes the routes like a team
	// key. botID is the launch's bot as the pool reads it (the walk's raw
	// botID).
	attachment *store.RunPoolAttachment
	botID      string
	// boundary is the resolution's boundary source (content_boundary.go): a
	// replay's frozen stamp, not the probe, decides whether the gateway
	// routes are attributed (gatewayRouting). The zero value is a launch.
	boundary boundarySource
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
// (llm_fallback=none) owes them one outside a runner pool: such a team's
// gateway route that nothing of its own funds is refused by name, never
// served by the pod's environment. A pool-stamped run without a key keeps the env-funded reading
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
	sealable, mayRide := attributed || undecidedOnPool, routed || undecidedOnPool
	replayHeldOut := ""
	switch frozen := probe.boundary.frozen; {
	case !probe.boundary.replay:
	case frozen.TeamGateway():
		// The launch attributed the gateway routes and sealed their key:
		// the frozen stamp decides, whatever the probe reads now (an attached
		// entry its RunID-blind read counts the run's own lease against, a
		// key deleted since). The walk asks every tier for the slot, and a
		// slot it cannot fill is refused by name (the boundary's strict arm)
		// — never left to the pod's environment the stamp refuses.
		attributed, sealable = routed, routed || undecidedOnPool
	case !launchSealedGateway(frozen):
		// The launch sealed no gateway key: a replay seals none either. The
		// routes ride the runner's environment gateway as at launch — a
		// resume reading a var its launch held undecided, a key created since,
		// a fork re-routing a node never move the run onto the team's key
		// under a stamp that holds no boundary.
		if attributed || undecidedOnPool {
			by := "the gateway slot"
			if hold.by != nil {
				by = hold.by.String()
			}
			replayHeldOut = fmt.Sprintf("%s is held out: the run's launch sealed no gateway key (content boundary %s), and a replay seals none its launch did not", by, frozenKind(frozen))
		}
		attributed, sealable, mayRide = false, false, false
	}
	if !attributed {
		keyed, walk = view, model.EffectiveProviders(view, walkProviderSet)
	}
	g := gatewayRoutes{
		view:          keyed,
		envFunded:     walk.OnlyEnvFunded(),
		sealable:      sealable,
		mayRide:       mayRide,
		note:          hold.note,
		repoOnly:      hold.repoOnly,
		replayHeldOut: replayHeldOut,
	}
	if attributed {
		g.attributedTo = hold.by
	}
	return g
}

// launchSealedGateway reports whether a frozen stamp records a gateway key
// the run's launch sealed: team_gateway always does, own_keys does when it
// names the pair's key or host (derivedContentBoundary: a key sealed for a
// route the walk could not decide, under a runner pool). A stamp written
// before the producer (nil) records none: a server of that time froze a
// routing pair order that could not name the gateway pair — its vocabulary
// refused `openai_compatible_key` (llmroute.validKeyProvider) and its default
// order (llmroute.Normalize's DefaultPairOrder) lacked it — so its whitelist
// held the gateway slot out of every tier, the attached pool's gateway want
// included, and no launch of that time sealed a gateway key.
func launchSealedGateway(b *store.RunContentBoundary) bool {
	return b != nil && (b.TeamGateway() || b.KeyID != "" || b.GatewayHost != "")
}

// frozenKind names a frozen stamp's kind for a sentence, "none" for a run
// stamped before the producer.
func frozenKind(b *store.RunContentBoundary) string {
	if b == nil || b.Kind == "" {
		return "none"
	}
	return b.Kind
}

// unsealedNote is the walk's reading, after the tier walk, of a gateway route
// the probe attributed to a key no tier then sealed — a platform key while a
// pool grant keeps the platform tier unconsulted, an attached pool's entry its
// draw could not serve now. Outside the content boundary (a launch derives
// own_keys for it) such routes ride the runner's environment gateway, as a held
// key that cannot seal leaves them (gatewayRoutes.note): the note names the key
// and why, and the unfunded refusal reads them as the environment's. "" when
// the case does not arise: nothing attributed to a key (the sovereign
// posture's owed routes included), the slot sealed, or the run held to its
// gateway (the boundary's strict arm names the slot).
func (g gatewayRoutes) unsealedNote(gatewaySealed, teamGateway, poolGranted bool) string {
	if g.attributedTo == nil || gatewaySealed || teamGateway {
		return ""
	}
	why := "no tier sealed it for this run (the tier lines above say why)"
	switch {
	case g.attributedTo.poolID != "":
		why = "its pool's draw served no entry now (the attached pool's line above says why)"
	case g.attributedTo.tier == "platform" && poolGranted:
		why = "the platform tier is not consulted once a pool grant serves the run"
	}
	return g.attributedTo.String() + " attributed them, but " + why
}

// gatewayHold is the probe's answer: whether a consulted tier holds a
// gateway key that funds the run, and the note naming what it held that
// cannot (gatewayRoutes.note).
type gatewayHold struct {
	funds    bool
	note     string
	repoOnly *heldGatewayKey
	// by is the key that funds the run when funds is set by a key the probe
	// read — what a gateway route attributed to it and then left unsealed is
	// named by (gatewayRoutes.unsealedNote).
	by *heldGatewayKey
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
// The ATTACHED pool (ADR-164) is one of those tiers, between the org's and
// the platform's as the walk visits them: a gateway entry it holds for this
// launch — the pool's eligibility and the entry's key row audiences, read
// without availability like a team key (attachedGatewayHold) — funds the
// routes like a team key, so the walk asks the pool for the slot and the run
// is held to the team's gateway when the draw seals it. Its key is lent, never
// opened here: the broker opens it at the grant.
//
// A tier whose keys cannot be listed is read as HOLDING one, said out loud:
// the walk then consults that tier, whose own read decides — the tenant's
// fails the launch, a shared tier's skips with its own line — where reading
// it as empty would send the team's routes to the pod's money in silence.
func (p *Publisher) gatewayKeyHold(ctx context.Context, tenantID, orgID, ownerID, audienceBotID string, sharedTiersBlocked bool, probe gatewayProbe) gatewayHold {
	if tenantID == "" {
		return gatewayHold{}
	}
	repo := secrets.LaunchRepoFromContext(ctx)
	slot := slotOfProvider(secrets.ProviderOpenAICompatible)
	unopened := ""
	var repoOnly *heldGatewayKey
	// visit answers for one tier: decided reports that the tier settles the
	// probe (a key that funds the run, the lists holding the slot out, or a
	// list that failed); otherwise the walk moves to the next tier, as the
	// walk's own fill would.
	visit := func(tier, scope, userID string) (hold gatewayHold, decided bool) {
		if p.apiKeys == nil {
			return gatewayHold{}, false
		}
		visible, err := p.apiKeys.ListByTeam(store.WithTenant(ctx, scope), scope, userID)
		if err != nil {
			if p.logger != nil {
				p.logger.Warn("cloudpublisher: %s tier key list for the gateway probe: %v — read as holding a gateway key: the walk consults the tier and its own read decides", tier, err)
			}
			return gatewayHold{funds: true, note: fmt.Sprintf("the %s tier's key list could not be read (%v), so the gateway probe read it as holding a gateway key and the walk's own read of that tier decided", tier, err)}, true
		}
		for _, c := range secrets.OrderedAPIKeyCandidates(visible, userID, []secrets.Provider{secrets.ProviderOpenAICompatible}, nil) {
			k := c.Key
			if !k.ServesBotForLaunch(audienceBotID) {
				continue
			}
			if !k.ServesRepoForLaunch(repo) {
				if probe.repoUnknown && repo == "" && repoOnly == nil {
					repoOnly = &heldGatewayKey{tier: tier, key: k}
				}
				continue
			}
			key := &heldGatewayKey{tier: tier, key: k}
			held := key.String() + " serves this launch"
			if !probe.lists.sealableHeldSlot(slot) {
				return gatewayHold{note: held + ", but " + slotHeldOutBy(probe.allowed, slot)}, true
			}
			if probe.opens == nil {
				return gatewayHold{funds: true, by: key}, true
			}
			if err := probe.opens(k); err != nil {
				if unopened == "" {
					unopened = held + fmt.Sprintf(", but its sealed secret does not open (%v)", err)
				}
				continue
			}
			return gatewayHold{funds: true, by: key}, true
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
		if hold, decided := p.attachedGatewayHold(ctx, orgID, tenantID, ownerID, slot, probe); decided {
			return hold
		} else if repoOnly == nil {
			repoOnly = hold.repoOnly
		}
		if p.platformAudience == nil || p.platformAudience.Get(ctx).Allows(orgID, tenantID) {
			if hold, decided := visit("platform", secrets.PlatformTenantID, ""); decided {
				return hold
			}
		}
	}
	return gatewayHold{note: unopened, repoOnly: repoOnly}
}

// attachedGatewayHold is gatewayKeyHold's attached-pool tier: decided when
// the attachment's pool holds a gateway entry for this launch (funding the
// routes, or held out by the run's frozen lists, noted like a team key), or
// when its metadata cannot be read (read as holding one, as a key list that
// failed is). Not decided when there is no attachment or no entry is there
// for the launch.
//
// It reads EXISTENCE and AUDIENCE, never availability — the team-key tier's
// doctrine: an entry the pool's eligibility admits (enabled, healthy, its
// pledge's bot filter, its lent row's own bots and repositories, the row
// still there) funds the routes whatever its ledger, cooldown, window or the
// attachment's draw caps say now. The walk's draw decides with the run's own
// identity (a resume's open lease and its day's unit are its own, which a
// RunID-blind preview counts against it): an entry that cannot serve then
// leaves the gateway slot empty and is named — refused under a frozen
// team_gateway, left to the pod's gateway outside it
// (gatewayRoutes.unsealedNote) — never decided by a read that does not know
// the run. A launch whose repository is unknown (the
// preview's) reports an entry its row's repository audience alone keeps out
// (gatewayRoutes.repoOnly), never reads it absent.
func (p *Publisher) attachedGatewayHold(ctx context.Context, orgID, tenantID, ownerID, slot string, probe gatewayProbe) (hold gatewayHold, decided bool) {
	att := probe.attachment
	if att == nil || att.PoolID == "" || p.credPool == nil {
		return gatewayHold{}, false
	}
	want := credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible)}
	unread := func(err error) (gatewayHold, bool) {
		if p.logger != nil {
			p.logger.Warn("cloudpublisher: attached pool %s metadata for the gateway probe: %v — read as holding a gateway entry: the walk consults the pool and its own draw decides", att.PoolID, err)
		}
		return gatewayHold{funds: true, note: fmt.Sprintf("the attached pool %s could not be read (%v), so the gateway probe read it as holding a gateway entry and the walk's own draw decided", att.PoolID, err)}, true
	}
	pv, err := p.credPool.Preview(ctx, p.attachedPoolRequest(ctx, orgID, tenantID, ownerID, probe.botID, att, []credpool.Credential{want}))
	if err != nil {
		return unread(err)
	}
	repo := secrets.LaunchRepoFromContext(ctx)
	var repoOnly *heldGatewayKey
	for _, c := range pv.Candidates {
		if c.Source != want.Source || c.Ref != want.Ref || c.PledgeID == "" || !entryThereForLaunch(c.Status) {
			continue
		}
		// The lent row: one deleted since the pledge was written lends
		// nothing; a row that cannot be read is read as holding one, as a
		// failed list is.
		k, err := p.credPool.LentKeyRow(ctx, c.PledgeID)
		switch {
		case errors.Is(err, secrets.ErrApiKeyNotFound):
			continue
		case err != nil:
			return unread(err)
		}
		why := lentAudience(k, probe.botID, repo)
		if why.Bot {
			continue
		}
		if why.Repo {
			if probe.repoUnknown && repo == "" && repoOnly == nil {
				repoOnly = &heldGatewayKey{tier: "pool", key: k, poolID: att.PoolID}
			}
			continue
		}
		entry := &heldGatewayKey{tier: "pool", key: k, poolID: att.PoolID}
		if !probe.lists.sealableHeldSlot(slot) {
			return gatewayHold{note: entry.String() + " serves this launch, but " + slotHeldOutBy(probe.allowed, slot)}, true
		}
		return gatewayHold{funds: true, by: entry}, true
	}
	return gatewayHold{repoOnly: repoOnly}, false
}

// entryThereForLaunch reports whether a preview candidate's status leaves the
// entry in the pool for this launch, availability aside: a paused pledge, an
// unhealthy one (its credential failing or gone) and one whose pledge's bot
// filter refuses the launch are not; busy, exhausted, cooling, out of hours,
// near limit and an unread capacity are only not available NOW.
func entryThereForLaunch(st credpool.Status) bool {
	switch st {
	case credpool.StatusPaused, credpool.StatusUnhealthy, credpool.StatusBotFiltered, credpool.StatusRepoFiltered, credpool.StatusNoFallbackConsent:
		return false
	}
	return true
}
