package cloudpublisher

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The unfunded-provider refusals, and the walk reading they are built on.
//
// A launch is refused when a provider some route of the run NAMES holds no
// credential the run may spend. The set those refusals read used to be
// `spendableProviders`, which returns its ZERO value as soon as one route of
// the workflow cannot be attributed (`!NarrowSafe`): a single unpinned
// `backend: claude_code` node therefore emptied the pinned set and disarmed
// the refusal for every OTHER pinned route — which then spent the runner
// pod's environment, the one funding `llm_fallback: none` exists to rule out.
//
// `spendableProviders` is right to fail OPEN: it NARROWS a request (what the
// run may spend, read by the stamp and the funding tiers), and an
// unreadable route may spend anything. Funding is the opposite direction, and
// `ProviderResolution.Providers` is the sound LOWER BOUND for it — every name
// in it is named by some RESOLVED route, so refusing an unfunded one is never
// a guess. The unattributable routes keep the compromise they are owed: they
// contribute no name, so no provider refusal names them, and `spend.pinned`
// stays nil for the stamp and the tiers where nil rightly means "may spend
// anything". The untrusted posture alone refuses them, by SITE
// (ProviderResolution.UnattributedSites): its runner decides what they spend.

// routeAttribution is the walk's reading the unfunded decision judges, read
// through the same walk and vocabulary the run-doc stamp uses, so no two
// surfaces can disagree about what a run targets.
type routeAttribution struct {
	// providers is the walk's lower bound: the providers some resolved route
	// names, sorted. A gateway route (`openai_compatible/…`) names the
	// openai_compatible provider exactly when the view attributes it to a key
	// (model.RouteView.GatewayKeyFunded: a tier the walk consults holds a
	// gateway key serving the launch, or the sovereign posture owes the route
	// one outside a runner pool) — and the refusals below then name it like
	// any provider, so a sovereign team's gateway route that nothing of its
	// own funds is refused by name (ADR-131 §5). Otherwise it is funded by
	// the runner's environment, acquires no credential, is counted in
	// EnvFunded and in gateway, and no provider refusal names it.
	providers []string
	// pins are the SITES that named each provider (a node id, `node#name`
	// for an authored fallback, `run-fallback#<stage>`, `supervisor:<name>`).
	pins map[string][]string
	// gateway are the sites whose route rides the openai_compatible gateway
	// (ProviderResolution.GatewaySites): funded by the runner's environment,
	// which only the untrusted posture refuses.
	gateway []string
	// deferred are the sites whose route the dispatching process decides
	// (ProviderResolution.DeferredSites): an `${…}` dial the runner expands
	// with its own environment, or a late run var. Only the untrusted
	// posture refuses them — its runner is a pool whose env block this
	// process cannot read.
	deferred []string
	// unattributed are the sites the walk could not attribute to a provider
	// (ProviderResolution.UnattributedSites): whatever such a route spends,
	// the runner decides it. Only the untrusted posture refuses them.
	unattributed []string
	// wireReads maps each site whose route may read an anthropic-wire slot
	// as the run's default credential to those slots
	// (ProviderResolution.WireDefaultSites), and sites maps each site to the
	// providers its routes name (ProviderResolution.Sites): what the untrusted
	// posture reads to tell a wire-default route its team funds from one the
	// runner refuses ambient.
	wireReads map[string][]string
	sites     map[string][]string
	// primary is the subset of providers a decided primary route names
	// (model.ProviderResolution.DecidedPrimary), with those sites: the set
	// the content boundary's strict arm refuses on.
	primary map[string][]string
}

// untrustedUnattributed are the unattributed sites the untrusted arm names on
// their own: a deferred site is already named, with its own remedy.
func (r routeAttribution) untrustedUnattributed() []string {
	deferred := siteSet(r.deferred)
	out := make([]string, 0, len(r.unattributed))
	for _, site := range r.unattributed {
		if !deferred[site] {
			out = append(out, site)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// unfundedWireDefault lists, rendered `site (slot, …)` and sorted, the sites
// whose route may spend an anthropic-wire slot as the run's DEFAULT credential
// — a claude_code route that pins no anthropic-wire provider, a route whose
// backend the runner picks — while funding holds none of the slots it may read
// and no anthropic-wire provider it names. On the runner such a route walks
// the wire's slot order over the run's sealed keys and, finding none, is
// refused ambient (the claude_code delegate's ambientStampRefusal). A route
// whose every slot the providers allowlist excludes reads an empty list and is
// refused the same way: no slot it may spend is ever sealed. A site already
// refused — gateway, deferred, unattributed, or naming a provider in
// unfunded — answers through its own remedy.
func (r routeAttribution) unfundedWireDefault(funded map[string]bool, unfunded []string) []string {
	refused := siteSet(r.gateway)
	for site := range siteSet(r.deferred) {
		refused[site] = true
	}
	for _, site := range r.unattributed {
		refused[site] = true
	}
	for _, prov := range unfunded {
		for _, site := range r.pins[prov] {
			refused[site] = true
		}
	}
	var out []string
	for site, slots := range r.wireReads {
		if refused[site] {
			continue
		}
		held := false
		for _, slot := range slots {
			held = held || funded[slot]
		}
		for _, prov := range r.sites[site] {
			held = held || (secrets.WireFamily(prov) == secrets.WireFamilyAnthropic && funded[prov])
		}
		switch {
		case held:
		case len(slots) == 0:
			out = append(out, site+" (no slot the providers allowlist admits)")
		default:
			out = append(out, fmt.Sprintf("%s (%s)", site, strings.Join(slots, ", ")))
		}
	}
	sort.Strings(out)
	return out
}

// siteSet is a site list as a set.
func siteSet(sites []string) map[string]bool {
	set := make(map[string]bool, len(sites))
	for _, site := range sites {
		set[site] = true
	}
	return set
}

// decidedProviders are the providers some route NOT in deferred names —
// the ones this process reads as the run will. A provider no site was
// recorded for is kept: nothing says it was deferred.
func (r routeAttribution) decidedProviders() []string {
	if len(r.deferred) == 0 {
		return r.providers
	}
	deferred := make(map[string]bool, len(r.deferred))
	for _, site := range r.deferred {
		deferred[site] = true
	}
	out := make([]string, 0, len(r.providers))
	for _, prov := range r.providers {
		sites := r.pins[prov]
		decided := len(sites) == 0
		for _, site := range sites {
			decided = decided || !deferred[site]
		}
		if decided {
			out = append(out, prov)
		}
	}
	return out
}

// attributedRoutes reads a program's routeAttribution.
func attributedRoutes(view model.RouteView) routeAttribution {
	res := model.EffectiveProviders(view, walkProviderSet)
	return routeAttribution{providers: res.Providers, pins: res.Pins, gateway: res.GatewaySites, deferred: res.DeferredSites,
		unattributed: res.UnattributedSites, wireReads: res.WireDefaultSites, sites: res.Sites, primary: res.DecidedPrimary}
}

// fundedProviderSet is what "funded" MEANS for the refusals below and for
// every surface that observes them: ONE reading of the two channels a run can
// be funded by — a provider key slot, and an OAuth forfait's provider.
//
// Both channels: a pinned key is exactly the case these gates must not refuse
// — every route pins one provider, a tier funded it for that pin, and reading
// the default slots alone would fail the launch claiming no tier holds a
// credential for it while the key is sealed in this bundle.
//
// The SOURCE of the two channels differs per surface (the launch reads its
// sealed bundle, the credential preview the fills it predicts — the
// doorServes / doorServesMetadata motif), the reading does not: a second copy
// of it is where one surface would start disagreeing with the other about
// what a funded provider is.
//
// The posture is part of the reading for ONE channel: under the untrusted
// posture a forfait funds nothing. The sandbox's egress proxy substitutes a
// bearer placeholder toward its credential's host and cannot hold an OAuth
// credential and its refresh flow, so the run's admission refuses the node
// that would spend one (ADR-162 decision 7, credroute.ErrCrossingRefused) —
// counting it here would admit, and meter, a run its runner refuses.
func fundedProviderSet(apiSlots []secrets.Provider, oauthKinds []string, posture unfundedPosture) map[string]bool {
	funded := make(map[string]bool, len(apiSlots)+len(oauthKinds))
	for _, prov := range apiSlots {
		funded[strings.ToLower(string(prov))] = true
	}
	if posture.Untrusted {
		return funded
	}
	for _, kind := range oauthKinds {
		if prov := providerOfOAuthKind(kind); prov != "" {
			funded[prov] = true
		}
	}
	return funded
}

// bundleFunding is fundedProviderSet on a SEALED bundle: the launch's own
// reading, the one its refusals are computed on.
func bundleFunding(bundle secrets.RunBundle, posture unfundedPosture) map[string]bool {
	kinds := make([]string, 0, len(bundle.OAuthCredentials))
	for kind := range bundle.OAuthCredentials {
		kinds = append(kinds, kind)
	}
	return fundedProviderSet(fundedAPIKeySlots(bundle), kinds, posture)
}

// unfundedAmong lists the providers of provs that funding holds nothing for,
// lowercased and sorted.
func unfundedAmong(provs []string, funded map[string]bool) []string {
	if len(provs) == 0 {
		return nil
	}
	out := make([]string, 0, len(provs))
	for _, prov := range provs {
		if p := strings.ToLower(prov); !funded[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// unfundedPosture is the ARMING of the refusals, read once per surface: the
// deployment knob (ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL), the team's
// sovereign `llm_fallback: none`, and the run's own trust. A run that cannot
// call a model spends nothing, and an env-funded run acquires nothing and
// answers to no team policy — so the arming, like the decision, has one
// producer.
type unfundedPosture struct {
	// Knob arms the deployment's refusal, permissive by design.
	Knob bool
	// Sovereign arms the team policy's, strict per provider.
	Sovereign bool
	// Untrusted arms the refusal of an untrusted (fork-lane) run, strict per
	// provider like the sovereign one, by construction rather than by
	// policy: the run executes on its lane's pool, so no shared tier (org,
	// pool, platform) is sealed into it (ADR-154 D13); the runner's
	// environment — its vendor keys and its pool's gateway — answers a
	// typed refusal, and a forfait cannot cross its sandbox (ADR-162
	// decision 7). The team's own vendor API keys are all that funds it.
	//
	// Three route classes are refused whatever the team holds. A gateway
	// route the runner's environment funds: the gateway it rides IS that
	// refused environment (an env-funded program does not lift the posture
	// either); a gateway route the team's own sealed gateway key funds
	// (model.RouteView.GatewayKeyFunded) names the openai_compatible
	// provider instead, judged by name like a vendor route. A deferred
	// route — an `${…}` dial, a late run var: the pool's runner expands it
	// with ITS environment, whose env block pins the routing dials ahead of
	// the platform record (sovereign-pools.md § Routing dials), so the
	// route this process reads is not the one the run takes, and a verdict
	// on it would admit, and meter, a run its runner then refuses. An
	// unattributed route — a prefix-less model, an `auto` or unknown step,
	// a subbot whose child bot the walk never reads, a supervisor with no
	// model: the walk names no provider for it, so no verdict says the
	// team's keys fund it. The cost is stated: a claude_code route with a
	// bare model that the team's Anthropic key would serve is refused too,
	// and the remedy — pin its provider — is one routing override.
	//
	// A fourth class is judged per site: a route that may spend an
	// anthropic-wire slot as the run's default credential (a claude_code
	// route that pins no wire provider, a route whose backend the runner
	// picks) is funded only when the team holds one of the slots it may read
	// or a wire provider it names — what the runner's slot-order walk serves
	// before it refuses the route ambient.
	Untrusted bool
	// Pool is the runner pool the run is stamped to, named by the untrusted
	// refusal's sentence ("" when the stamp is not known).
	Pool string
	// TeamGateway arms the content boundary's, strict per provider on the
	// decided primary routes: a run held to its team's gateway (ADR-131 §4)
	// carries AmbientLLMRefused, so every environment fallback of it answers a
	// typed refusal — the pod's env funds none of its routes, and the knob's
	// tolerance of an unfunded route ("the env may fund it") does not hold for
	// it. A node's decided route nothing in the bundle funds could only die at
	// the runner.
	TeamGateway bool
}

// unfundedPostureFor arms the refusals for one run: sovereign is the team's
// llm_fallback=none, teamGateway the run's content boundary (derived at
// launch, frozen at resume — the value the bundle's ambient refusal reads),
// trust the run's own (store.RunTrust.AmbientLLMRefused arms the untrusted
// posture) and runnerPool the pool it is stamped to.
func (p *Publisher) unfundedPostureFor(wf *ir.Workflow, sovereign, teamGateway, envFunded bool, trust store.RunTrust, runnerPool string) unfundedPosture {
	usesLLM := wf != nil && wf.UsesLLM()
	return unfundedPosture{
		Knob:        p.requireLLMCredential && usesLLM,
		Sovereign:   sovereign && !envFunded && usesLLM,
		Untrusted:   trust.AmbientLLMRefused() && usesLLM,
		Pool:        runnerPool,
		TeamGateway: teamGateway && !envFunded && usesLLM,
	}
}

// unfundedRefusalFor is THE unfunded decision, every arm of it: the
// providers of attributed that funding holds nothing for, judged by the armed
// postures in the launch's own order — the untrusted posture first, then the
// deployment knob, then the sovereign policy — and, where no NAME is left to
// refuse, the strict postures' refusal of a run they can attribute nothing of
// while the run holds nothing at all (holdsNothing: !holdsDefaultLLMCredential
// on the launch's sealed bundle, its twin on the preview's metadata — an
// unattributable route is unpinned, so only a default credential, never the
// gateway key, funds it). The FIRST refusal answers for the run, nil meaning
// "this run starts".
//
// The untrusted posture answers first because its remedy is the only true
// one for that run: the knob's sentence offers the runner's environment as a
// way out, and an untrusted run is refused that environment. Under it,
// holding nothing means holding nothing that FUNDS — a sealed forfait is not
// funding there (fundedProviderSet), so the funded set itself is the answer,
// read without the gateway slot: a gateway key never pays an unattributed
// route (holdsDefaultLLMCredential).
//
// The pre-metering refusal of a fork run (the launch gate's funding arm, in
// pkg/server) reads this producer through the credential preview
// (Publisher.FundingRefusal, Publisher.ResumeFundingRefusal), so the gate
// that refuses before metering, the preview that mirrors it and the launch
// that would refuse at publish judge one decision.
//
// The launch RETURNS it and the credential preview PRINTS it, through this
// one producer and its one sentence: a surface that re-derived either would
// be a second, weaker reading of a decision it only observes, free to warn
// where the launch starts and to stay silent where it refuses (#2485). An arm
// kept outside this producer is exactly that silence, for that arm alone.
//
// heldOut names, per provider, what a consulted tier held for it that did not
// seal (gatewayRoutes.heldOut): a refusal naming that provider carries it, so
// the sentence never claims no tier holds what one does. routes.primary is
// the attributed set's decided primary routes (attributedRoutes), the content
// boundary's strict set.
func unfundedRefusalFor(tenant string, routes routeAttribution, funded map[string]bool, holdsNothing bool, posture unfundedPosture, heldOut map[string]string) *UnfundedProvidersError {
	attributed, sites, primary := routes.providers, routes.pins, routes.primary
	if posture.Untrusted {
		holdsNothing = true
		for prov := range funded {
			holdsNothing = holdsNothing && prov == string(secrets.ProviderOpenAICompatible)
		}
	}
	unfunded := unfundedAmong(attributed, funded)
	if posture.Untrusted {
		// A provider only deferred routes name is this process's expansion
		// of them, not a provider the run spends: the refusal names those
		// routes to pin, never a key to provision for it.
		unfunded = unfundedAmong(routes.decidedProviders(), funded)
		unattributed := routes.untrustedUnattributed()
		wire := routes.unfundedWireDefault(funded, unfunded)
		if len(unfunded) > 0 || len(routes.gateway) > 0 || len(routes.deferred) > 0 || len(unattributed) > 0 || len(wire) > 0 {
			// The untrusted arm answers for every route class it cannot fund
			// at once — an unfunded provider, a gateway route, a deferred
			// route, an unattributed route, an unfunded wire-default read —
			// so the remedy it names is the whole of it.
			refusal := newUnfundedProvidersError(tenant, unfunded, sites, heldOut, posture.Sovereign)
			refusal.Untrusted, refusal.Pool = true, posture.Pool
			refusal.Gateway, refusal.Deferred = routes.gateway, routes.deferred
			refusal.Unattributed, refusal.WireDefault = unattributed, wire
			return refusal
		}
	}
	if len(unfunded) == 0 {
		// The attribution compromise, stated on the attributed set: a
		// workflow NO route of which the walk can read still refuses when the
		// team holds nothing at all — the pod's platform env would be the
		// only funding left, which is what the policy exists to make
		// impossible. A team holding its own credentials keeps the knob's
		// compromise for those routes; the ones the walk DID read are decided
		// by name below, and this arm is reachable only where that decision
		// found nothing to refuse — the launch's own order.
		if (posture.Sovereign || posture.Untrusted) && len(attributed) == 0 && holdsNothing {
			return &UnfundedProvidersError{Tenant: tenant, Sovereign: posture.Sovereign, Untrusted: posture.Untrusted, Pool: posture.Pool, Unattributable: true}
		}
		return nil
	}
	// The content boundary first: under team_gateway the environment the
	// knob's sentence offers is refused, so a node's DECIDED primary route
	// nothing funds is refused by name, the sentence naming the boundary. A
	// provider only a rescue route or an env dial names is not (the caller
	// warns, teamGatewayTolerated): a rescue fires only once its node's
	// funded route failed, and the runner's pod expands a dial with its own
	// environment — a runner pool's to the gateway the sealed pair funds. The
	// knob, whose sentence offers the environment, stays out of a
	// team_gateway run.
	if posture.TeamGateway {
		if strict := unfundedAmong(sortedProviders(primary), funded); len(strict) > 0 {
			refusal := newUnfundedProvidersError(tenant, strict, primary, heldOut, posture.Sovereign)
			refusal.TeamGateway = true
			return refusal
		}
	} else if posture.Knob && len(unfunded) == len(attributed) {
		// The deployment knob stays permissive: one funded provider lets the
		// run start — the other route's failure is the run's to report. Only
		// a run whose EVERY attributed provider is unfunded cannot start at
		// all.
		return newUnfundedProvidersError(tenant, unfunded, sites, heldOut, false)
	}
	// The sovereign policy arms the same refusal STRICTLY, without the
	// deployment knob: under llm_fallback=none every provider a route names
	// must hold a credential of the team's own — one funded provider does not
	// excuse another pinned route, whose only remaining funding would be the
	// runner pod's platform env.
	if posture.Sovereign {
		return newUnfundedProvidersError(tenant, unfunded, sites, heldOut, true)
	}
	return nil
}

// sortedProviders lists a provider → sites map's providers, sorted.
func sortedProviders(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// teamGatewayTolerated names what the content boundary's strict arm leaves
// unrefused on a team_gateway run: each unfunded provider no decided primary
// route names — only a rescue route or an env dial does — with the sites
// naming it. "" when there is none. The launch warns it, the preview mirrors
// the warning.
func teamGatewayTolerated(attributed []string, sites, primary map[string][]string, funded map[string]bool) string {
	var named []string
	for _, prov := range unfundedAmong(attributed, funded) {
		if _, strict := primary[prov]; strict {
			continue
		}
		entry := prov
		if at := sites[prov]; len(at) > 0 {
			entry = fmt.Sprintf("%s (named by %s)", prov, strings.Join(at, ", "))
		}
		named = append(named, entry)
	}
	return strings.Join(named, "; ")
}

// UnfundedProvidersError is the typed refusal of a launch whose attributed
// providers no credential of the run's funds.
//
// It carries the FACTS — which providers, which sites named each, which
// posture refused — so a reader answers with errors.As instead of parsing the
// sentence: a caller that reconstructs "which provider" from the message is a
// second, weaker parser of this decision. Unwraps to runview.ErrNoLLMCredential,
// which every existing reader already matches with errors.Is.
type UnfundedProvidersError struct {
	// Tenant is the team the launch resolved credentials for.
	Tenant string
	// Providers are the unfunded attributed providers, sorted.
	Providers []string
	// Sites maps each provider to the route sites that named it
	// (ProviderResolution.Pins), empty when the walk recorded none.
	Sites map[string][]string
	// Sovereign marks the `llm_fallback: none` arm — the team's own
	// credentials are the only funding — as opposed to the deployment knob.
	Sovereign bool
	// Untrusted marks the untrusted (fork-lane) arm: the run spends the
	// team's own vendor API keys alone — no shared tier is sealed into a
	// pool run, the runner's environment is refused and a forfait cannot
	// cross its sandbox. It answers before the two others, so it carries the
	// run's remedy whatever else is armed.
	Untrusted bool
	// Gateway lists, on the untrusted arm, the sites whose route rides the
	// openai_compatible gateway the runner's environment funds: nothing funds
	// one on a fork run (the runner's gateway is the refused environment, and
	// no gateway key of the team's own is sealed for it — a route that key
	// funds names the openai_compatible provider instead). Sorted.
	Gateway []string
	// Deferred lists, on the untrusted arm, the sites whose route the pool's
	// runner decides — an `${…}` dial expanded with the pool's own
	// environment, or a late run var — so no verdict this process reaches is
	// the route the run takes. Sorted.
	Deferred []string
	// Unattributed lists, on the untrusted arm, the sites whose route the walk
	// could not attribute to a provider (a deferred site excluded: it is
	// named in Deferred). Sorted.
	Unattributed []string
	// WireDefault lists, on the untrusted arm, the sites whose route may spend
	// an anthropic-wire slot as the run's default credential while the team
	// holds none of the slots it may read, each rendered `site (slot, …)`.
	// Sorted.
	WireDefault []string
	// Pool is the runner pool the untrusted run is stamped to ("" unknown).
	Pool string
	// TeamGateway marks the content boundary's arm: the run is held to its
	// team's gateway, so no environment fallback funds an unfunded route.
	// Its sentence names the boundary unless Sovereign says more.
	TeamGateway bool
	// Unattributable marks the strict arm that names NO provider: the walk
	// could attribute none of this workflow's routes and the run holds
	// nothing that funds it, so Providers and Sites are empty by
	// construction.
	Unattributable bool
	// HeldOut maps a named provider to what a consulted tier held for it
	// that did not seal, and why (the gateway probe's note): the key, or the
	// tier read as holding one. Empty when no tier held anything for it.
	HeldOut map[string]string
}

func (e *UnfundedProvidersError) Unwrap() error { return runview.ErrNoLLMCredential }

func (e *UnfundedProvidersError) Error() string {
	if e.Untrusted {
		pool := "its fork lane's runner pool"
		if e.Pool != "" {
			pool = fmt.Sprintf("runner pool %q", e.Pool)
		}
		why := "no shared tier (org, pool, platform) is sealed into a pool run (ADR-154 D13), the runner's environment and its gateway are refused, and a forfait cannot cross the run's sandbox (ADR-162 decision 7)"
		if e.Unattributable {
			return fmt.Sprintf("cloudpublisher: %v: tenant %s's untrusted (fork-lane) run on %s holds no API key of the team's own, and no model route of this workflow names a provider one could fund — %s; pin the bot's routes to a vendor provider and provision the team's API key for it (a gateway key funds only the gateway routes it is attributed, never an unattributed one)",
				runview.ErrNoLLMCredential, e.Tenant, pool, why)
		}
		remedies := make([]string, 0, 3)
		if len(e.Providers) > 0 {
			remedies = append(remedies, "provision a team API key for: "+strings.Join(e.namedProviders(), "; "))
		}
		if len(e.Gateway) > 0 {
			remedies = append(remedies, fmt.Sprintf("the routes of %s ride the openai_compatible gateway, which funds no fork run on the runner's environment — the runner's gateway is the refused environment, and no gateway key of the team's own is sealed for them: provision the team's gateway key for this bot and repository, or route them to a vendor provider the team holds an API key for",
				strings.Join(e.Gateway, ", ")))
		}
		if len(e.Deferred) > 0 {
			remedies = append(remedies, fmt.Sprintf("the routes of %s are decided on the runner — an ${…} dial the pool's runner expands with its own environment, whose env block pins the routing dials ahead of the platform record, or a run var this launch leaves undecided — so no verdict reached before the run is the route it takes: give them literal routes until a run's settings arrive frozen at claim (ADR-132) — %s",
				strings.Join(e.Deferred, ", "), overrideReach))
		}
		if len(e.Unattributed) > 0 {
			remedies = append(remedies, fmt.Sprintf("the routes of %s name no provider this launch can read — a model with no provider/ prefix, an auto, empty or unknown provider step, a subbot whose child bot is not read here, a supervisor with no model — so the pool's runner decides what they spend: pin each to a vendor provider the team holds an API key for, by a `provider:` hint or a provider/ model prefix — %s",
				strings.Join(e.Unattributed, ", "), overrideReach))
		}
		if len(e.WireDefault) > 0 {
			remedies = append(remedies, fmt.Sprintf("the routes of %s may spend the anthropic wire's default credential — a claude_code route that pins no anthropic-wire provider, or a route whose backend the runner picks — and the team holds an API key for none of the slots listed, or the providers allowlist admits none: provision one the allowlist admits, or pin the route's backend and provider",
				strings.Join(e.WireDefault, ", ")))
		}
		return fmt.Sprintf("cloudpublisher: %v: tenant %s's untrusted (fork-lane) run on %s spends the team's own vendor API keys alone — %s; %s",
			runview.ErrNoLLMCredential, e.Tenant, pool, why, strings.Join(remedies, "; and "))
	}
	// The arm that names NO provider: nothing to walk, and the remedy is a
	// credential or a pinned route rather than a provider to provision.
	if e.Unattributable {
		return fmt.Sprintf("cloudpublisher: %v: tenant %s runs with llm_fallback=none and holds no credential of its own, and this workflow's model routes cannot be attributed to a provider — provision a team credential, or pin a model route the policy can check",
			runview.ErrNoLLMCredential, e.Tenant)
	}
	named := e.namedProviders()
	if e.TeamGateway && !e.Sovereign {
		return fmt.Sprintf("cloudpublisher: %v: this run is held to its team's gateway (content boundary team_gateway, ADR-131 §4) — every environment fallback of it is refused, so every provider a node's own decided route names must be funded by a credential the run carries; provision one for tenant %s: %s",
			runview.ErrNoLLMCredential, e.Tenant, strings.Join(named, "; "))
	}
	if e.Sovereign {
		return fmt.Sprintf("cloudpublisher: %v: tenant %s runs with llm_fallback=none — no shared tier (org, pool, platform) is consulted, so every provider a route names must hold a credential of the team's own; provision: %s",
			runview.ErrNoLLMCredential, e.Tenant, strings.Join(named, "; "))
	}
	return fmt.Sprintf("cloudpublisher: %v: every provider the LLM routes of this run name is unfunded — no tier (byok, oauth-forfait, pool, platform) sealed a credential for %s; provision one for tenant %s, or unset ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL to let the runner fall back to its env",
		runview.ErrNoLLMCredential, strings.Join(named, "; "), e.Tenant)
}

// overrideReach is where the untrusted remedy says a route is pinned. An
// integration's routing override reaches a node's own route alone (ForNode:
// an agent, a judge, an llm router, a recovery rung's synthetic agent); the
// sites the refusal may also name — a sub-bot node, a supervisor, an authored
// `fallbacks:` route (`node#name`), an interaction or direct generation, a
// run-level fallback stage (`run-fallback#<stage>`) — are pinned by the bot's
// own text or the launch's fallback chain, so a remedy offering the override
// for them names a lever that cannot lift the refusal.
const overrideReach = "an integration's routing overrides pin a node's own route (provider, model and backend) and nothing else: a sub-bot node, a supervisor, a fallbacks: route (node#name), an interaction generation or a run-level fallback stage (run-fallback#…) is pinned in the bot's own text or the launch's fallback chain"

// namedProviders renders each refused provider with the route sites that
// named it, in the refusal's sorted order.
func (e *UnfundedProvidersError) namedProviders() []string {
	named := make([]string, 0, len(e.Providers))
	for _, prov := range e.Providers {
		entry := prov
		if sites := e.Sites[prov]; len(sites) > 0 {
			entry = fmt.Sprintf("%s (named by %s)", prov, strings.Join(sites, ", "))
		}
		if held := e.HeldOut[prov]; held != "" {
			entry += " — " + held
		}
		named = append(named, entry)
	}
	return named
}

// newUnfundedProvidersError builds the refusal, keeping only the sites and
// held-key notes of the providers it names: a refusal that carried the whole
// pin map would hand the caller route sites its own refusal says nothing
// about.
func newUnfundedProvidersError(tenant string, unfunded []string, pins map[string][]string, heldOut map[string]string, sovereign bool) *UnfundedProvidersError {
	sites := make(map[string][]string, len(unfunded))
	var held map[string]string
	for _, prov := range unfunded {
		if named := pins[prov]; len(named) > 0 {
			sites[prov] = named
		}
		if note := heldOut[prov]; note != "" {
			if held == nil {
				held = map[string]string{}
			}
			held[prov] = note
		}
	}
	return &UnfundedProvidersError{Tenant: tenant, Providers: unfunded, Sites: sites, Sovereign: sovereign, HeldOut: held}
}
