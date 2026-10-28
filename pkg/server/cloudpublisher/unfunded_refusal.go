package cloudpublisher

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
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

// unfundedAmong lists the providers of provs the bundle funds nothing of,
// sorted. The ONE funding reading, shared by both refusal arms: a second copy
// of it is where the next arm would disagree about what "funded" means.
func unfundedAmong(provs []string, bundle secrets.RunBundle) []string {
	if len(provs) == 0 {
		return nil
	}
	funded := make(map[string]bool, len(bundle.APIKeys)+len(bundle.OAuthCredentials))
	// Both channels: a pinned key is exactly the case these gates must not
	// refuse — every route pins one provider, a tier funded it for that pin,
	// and reading APIKeys alone would fail the launch claiming no tier holds
	// a credential for it while the key is sealed in this bundle.
	for _, prov := range fundedAPIKeySlots(bundle) {
		funded[strings.ToLower(string(prov))] = true
	}
	for kind := range bundle.OAuthCredentials {
		if prov := providerOfOAuthKind(kind); prov != "" {
			funded[prov] = true
		}
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
}

func (e *UnfundedProvidersError) Unwrap() error { return runview.ErrNoLLMCredential }

func (e *UnfundedProvidersError) Error() string {
	named := make([]string, 0, len(e.Providers))
	for _, prov := range e.Providers {
		if sites := e.Sites[prov]; len(sites) > 0 {
			named = append(named, fmt.Sprintf("%s (named by %s)", prov, strings.Join(sites, ", ")))
			continue
		}
		named = append(named, prov)
	}
	if e.Sovereign {
		return fmt.Sprintf("cloudpublisher: %v: tenant %s runs with llm_fallback=none — no shared tier (org, pool, platform) is consulted, so every provider a route names must hold a credential of the team's own; provision: %s",
			runview.ErrNoLLMCredential, e.Tenant, strings.Join(named, "; "))
	}
	return fmt.Sprintf("cloudpublisher: %v: every provider the LLM routes of this run name is unfunded — no tier (byok, oauth-forfait, pool, platform) holds a credential for %s; provision one for tenant %s, or unset ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL to let the runner fall back to its env",
		runview.ErrNoLLMCredential, strings.Join(named, "; "), e.Tenant)
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
