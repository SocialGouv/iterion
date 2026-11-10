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
// contribute no name, so they are never refused, and `spend.pinned` stays nil
// for the stamp and the tiers where nil rightly means "may spend anything".

// attributedRoutes is the walk's lower bound: the providers some resolved
// route names, and the SITES that named each (a node id, `node#name` for an
// authored fallback, `run-fallback#<stage>`, `supervisor:<name>`). Read
// through the same walk and vocabulary the pool's wants derivation and the
// run-doc stamp use, so no two surfaces can disagree about what a run
// targets. A gateway route (`openai_compatible/…`) names no provider here —
// it is funded by the runner's environment, acquires no credential, and is
// counted in EnvFunded instead — so no refusal below can ever name it.
func attributedRoutes(view model.RouteView) ([]string, map[string][]string) {
	res := model.EffectiveProviders(view, knownPoolProviders)
	return res.Providers, res.Pins
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
	// decision 7). The team's own API keys are all that funds it. An
	// env-funded program does not lift it: the gateway such a route rides
	// IS that refused environment, and the walk acquires nothing for it.
	Untrusted bool
	// Pool is the runner pool the run is stamped to, named by the untrusted
	// refusal's sentence ("" when the stamp is not known).
	Pool string
}

func (p *Publisher) unfundedPostureFor(wf *ir.Workflow, sovereign, envFunded bool, trust store.RunTrust, runnerPool string) unfundedPosture {
	usesLLM := wf != nil && wf.UsesLLM()
	return unfundedPosture{
		Knob:      p.requireLLMCredential && usesLLM,
		Sovereign: sovereign && !envFunded && usesLLM,
		Untrusted: trust.AmbientLLMRefused() && usesLLM,
		Pool:      runnerPool,
	}
}

// unfundedRefusalFor is THE unfunded decision, every arm of it: the
// providers of attributed that funding holds nothing for, judged by the armed
// postures in the launch's own order — the untrusted posture first, then the
// deployment knob, then the sovereign policy — and, where no NAME is left to
// refuse, the strict postures' refusal of a run they can attribute nothing of
// while the run holds nothing at all (holdsNothing: the launch's
// `!holdsLLMCredential(bundle)`, the preview's metadata twin). The FIRST
// refusal answers for the run, nil meaning "this run starts".
//
// The untrusted posture answers first because its remedy is the only true
// one for that run: the knob's sentence offers the runner's environment as a
// way out, and an untrusted run is refused that environment. Under it,
// holding nothing means holding nothing that FUNDS — a sealed forfait is not
// funding there (fundedProviderSet), so the funded set itself is the answer.
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
func unfundedRefusalFor(tenant string, attributed []string, sites map[string][]string, funded map[string]bool, holdsNothing bool, posture unfundedPosture) *UnfundedProvidersError {
	if posture.Untrusted {
		holdsNothing = len(funded) == 0
	}
	unfunded := unfundedAmong(attributed, funded)
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
	if posture.Untrusted {
		refusal := newUnfundedProvidersError(tenant, unfunded, sites, posture.Sovereign)
		refusal.Untrusted, refusal.Pool = true, posture.Pool
		return refusal
	}
	// The deployment knob stays permissive: one funded provider lets the run
	// start — the other route's failure is the run's to report. Only a run
	// whose EVERY attributed provider is unfunded cannot start at all.
	if posture.Knob && len(unfunded) == len(attributed) {
		return newUnfundedProvidersError(tenant, unfunded, sites, false)
	}
	// The sovereign policy arms the same refusal STRICTLY, without the
	// deployment knob: under llm_fallback=none every provider a route names
	// must hold a credential of the team's own — one funded provider does not
	// excuse another pinned route, whose only remaining funding would be the
	// runner pod's platform env.
	if posture.Sovereign {
		return newUnfundedProvidersError(tenant, unfunded, sites, true)
	}
	return nil
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
	// team's own API keys alone — no shared tier is sealed into a pool run,
	// the runner's environment is refused and a forfait cannot cross its
	// sandbox. It answers before the two others, so it carries the run's
	// remedy whatever else is armed.
	Untrusted bool
	// Pool is the runner pool the untrusted run is stamped to ("" unknown).
	Pool string
	// Unattributable marks the strict arm that names NO provider: the walk
	// could attribute none of this workflow's routes and the run holds
	// nothing that funds it, so Providers and Sites are empty by
	// construction.
	Unattributable bool
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
			return fmt.Sprintf("cloudpublisher: %v: tenant %s's untrusted (fork-lane) run on %s holds no API key of the team's own, and no model route of this workflow names a provider one could fund — %s; provision a team API key (with its gateway base URL where the team uses one) for the provider the bot's routes pin",
				runview.ErrNoLLMCredential, e.Tenant, pool, why)
		}
		return fmt.Sprintf("cloudpublisher: %v: tenant %s's untrusted (fork-lane) run on %s spends the team's own API keys alone — %s; provision a team API key for: %s",
			runview.ErrNoLLMCredential, e.Tenant, pool, why, strings.Join(e.namedProviders(), "; "))
	}
	// The arm that names NO provider: nothing to walk, and the remedy is a
	// credential or a pinned route rather than a provider to provision.
	if e.Unattributable {
		return fmt.Sprintf("cloudpublisher: %v: tenant %s runs with llm_fallback=none and holds no credential of its own, and this workflow's model routes cannot be attributed to a provider — provision a team credential, or pin a model route the policy can check",
			runview.ErrNoLLMCredential, e.Tenant)
	}
	named := e.namedProviders()
	if e.Sovereign {
		return fmt.Sprintf("cloudpublisher: %v: tenant %s runs with llm_fallback=none — no shared tier (org, pool, platform) is consulted, so every provider a route names must hold a credential of the team's own; provision: %s",
			runview.ErrNoLLMCredential, e.Tenant, strings.Join(named, "; "))
	}
	return fmt.Sprintf("cloudpublisher: %v: every provider the LLM routes of this run name is unfunded — no tier (byok, oauth-forfait, pool, platform) holds a credential for %s; provision one for tenant %s, or unset ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL to let the runner fall back to its env",
		runview.ErrNoLLMCredential, strings.Join(named, "; "), e.Tenant)
}

// namedProviders renders each refused provider with the route sites that
// named it, in the refusal's sorted order.
func (e *UnfundedProvidersError) namedProviders() []string {
	named := make([]string, 0, len(e.Providers))
	for _, prov := range e.Providers {
		if sites := e.Sites[prov]; len(sites) > 0 {
			named = append(named, fmt.Sprintf("%s (named by %s)", prov, strings.Join(sites, ", ")))
			continue
		}
		named = append(named, prov)
	}
	return named
}

// newUnfundedProvidersError builds the refusal, keeping only the sites of the
// providers it names: a refusal that carried the whole pin map would hand the
// caller route sites its own refusal says nothing about.
func newUnfundedProvidersError(tenant string, unfunded []string, pins map[string][]string, sovereign bool) *UnfundedProvidersError {
	sites := make(map[string][]string, len(unfunded))
	for _, prov := range unfunded {
		if named := pins[prov]; len(named) > 0 {
			sites[prov] = named
		}
	}
	return &UnfundedProvidersError{Tenant: tenant, Providers: unfunded, Sites: sites, Sovereign: sovereign}
}
