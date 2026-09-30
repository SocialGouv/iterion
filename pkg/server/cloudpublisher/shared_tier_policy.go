package cloudpublisher

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// sharedTierPolicy is the deployment's ordering of the shared tiers' (org,
// platform) credentials, read ONCE per resolution: the org fill, the platform
// fill, the restore and the preview of one launch must apply the same answer,
// and a settings write landing between two of them would split it.
type sharedTierPolicy struct {
	// keysFirst puts a tier's API keys before its forfaits on a wire family.
	keysFirst bool
	// facade says whether a facade key (z.ai, Moonshot) may be the anthropic
	// wire's default — see platformcfg.PlatformCredentials.FacadeDefault.
	facade platformcfg.FacadePolicy
}

func (p *Publisher) sharedTierPolicyFor(ctx context.Context) sharedTierPolicy {
	var rec *platformcfg.PlatformCredentials
	if p.platformAudience != nil {
		rec = p.platformAudience.Get(ctx)
	}
	return sharedTierPolicy{keysFirst: rec.PrefersKeys(), facade: rec.Facade()}
}

// inOrder runs one tier's forfait and key passes in the policy's order.
func (pol sharedTierPolicy) inOrder(forfaits, keys func()) {
	if pol.keysFirst {
		keys()
		forfaits()
		return
	}
	forfaits()
	keys()
}

// facadeMayDefault reports whether, in the tier `native` describes, a facade
// key may be the anthropic wire's default. `native` is only asked under
// `auto`, for a facade provider on a free family — once per resolution: at
// most one forfait list and one key list per tier.
func (pol sharedTierPolicy) facadeMayDefault(native *tierNative) bool {
	switch pol.facade {
	case platformcfg.FacadeAlways:
		return true
	case platformcfg.FacadeNever:
		return false
	}
	return !native.holds()
}

// isFacadeProvider names the providers that ride the anthropic wire without
// being Anthropic: they answer a claude id with their own model.
func isFacadeProvider(prov secrets.Provider) bool {
	return prov != secrets.ProviderAnthropic &&
		secrets.WireFamily(string(prov)) == secrets.WireFamily(string(secrets.ProviderAnthropic))
}

// sealOutcome is where a shared tier puts one API key.
type sealOutcome int

const (
	// sealNone: the key is not sealed for this run.
	sealNone sealOutcome = iota
	// sealDefault: the key fills its wire family — what every unpinned node
	// of the run spends.
	sealDefault
	// sealPinnedOnly: the key rides secrets.RunBundle.PinnedAPIKeys and serves
	// only the routes that name its provider.
	sealPinnedOnly
)

// sealDecision is the one judge of whether, and in which channel, a shared
// tier seals its API key of prov: the key fill, the refused-key memory the
// restore reads, the restore itself and the preview all ask it, so none can
// hand out what another withholds.
//
// A free family takes the key as its default — unless it is a facade the
// policy keeps off the default, which then serves a route naming it, or
// nobody. A taken family admits the key only for the routes that name it.
func sealDecision(prov secrets.Provider, familyTaken, pinned bool, facadeMayDefault func() bool) sealOutcome {
	if !familyTaken {
		if isFacadeProvider(prov) && !facadeMayDefault() {
			if pinned {
				return sealPinnedOnly
			}
			return sealNone
		}
		return sealDefault
	}
	if pinned {
		return sealPinnedOnly
	}
	return sealNone
}

// fillAPIKeySlotAs seals a key in the channel sealDecision chose. Pinned-only
// never marks the family: it did not fill it, and on a FREE family the
// tier's forfait must still be able to.
func fillAPIKeySlotAs(bundle *secrets.RunBundle, taken map[string]bool, prov secrets.Provider, plaintext string, outcome sealOutcome) (pinnedOnly bool) {
	if outcome == sealPinnedOnly {
		if bundle.PinnedAPIKeys == nil {
			bundle.PinnedAPIKeys = map[secrets.Provider]string{}
		}
		bundle.PinnedAPIKeys[prov] = plaintext
		return true
	}
	return fillAPIKeySlot(bundle, taken, prov, plaintext)
}

// tierNative answers, lazily and once, whether a shared tier holds an
// Anthropic-native credential for the anthropic wire: a Claude forfait in any
// window state, or an anthropic key its workload audience admits for the
// launch, in any meter state. `auto` keeps a facade off the default of such a
// tier — including while that credential is closed, which parks the run on it
// rather than switching it to another vendor.
type tierNative struct {
	probe func() bool
	done  bool
	value bool
}

func (t *tierNative) holds() bool {
	if t == nil || t.probe == nil {
		return false
	}
	if !t.done {
		t.value, t.done = t.probe(), true
	}
	return t.value
}

// newTierNative builds the probe for the tier whose forfaits live under
// forfaitOwner and whose keys live under keyScope. A store that cannot answer
// counts as holding NONE — the platform tier's rule for a degraded read: the
// tier a deployment runs on must not lose its only credential to a store
// blip. It is said out loud, since a facade may then take the default.
func (p *Publisher) newTierNative(ctx context.Context, tier, forfaitOwner, keyScope, botID string) *tierNative {
	return &tierNative{probe: func() bool {
		if p.oauthForfait != nil {
			recs, err := p.oauthForfait.ListByUser(ctx, forfaitOwner)
			if err != nil {
				p.logger.Warn("cloudpublisher: %s tier forfait list for the facade policy: %v — read as holding none: a facade key may take the anthropic wire's default for this launch", tier, err)
				return false
			}
			for _, rec := range recs {
				if rec.Kind == secrets.OAuthKindClaudeCode {
					return true
				}
			}
		}
		if p.apiKeys != nil {
			visible, err := p.apiKeys.ListByTeam(store.WithTenant(ctx, keyScope), keyScope, "")
			if err != nil {
				p.logger.Warn("cloudpublisher: %s tier key list for the facade policy: %v — read as holding none: a facade key may take the anthropic wire's default for this launch", tier, err)
				return false
			}
			for _, k := range visible {
				if k.Provider == secrets.ProviderAnthropic && k.ServesBotForLaunch(botID) {
					return true
				}
			}
		}
		return false
	}}
}

// holdsLLMCredential reports whether the bundle carries anything an LLM route
// can spend — a default key, a key pinned for a route, or a forfait. A bundle
// holding only a pinned key is still sealed: that key is some route's only
// credential.
func holdsLLMCredential(b secrets.RunBundle) bool {
	return len(b.APIKeys) > 0 || len(b.PinnedAPIKeys) > 0 || len(b.OAuthCredentials) > 0
}

// holdsDefaultLLMCredential reports whether an UNPINNED route has something
// to spend. The pool is consulted when it has not: a pinned-only key serves
// the routes that name its provider and nobody else.
func holdsDefaultLLMCredential(b secrets.RunBundle) bool {
	return len(b.APIKeys) > 0 || len(b.OAuthCredentials) > 0
}

// routeOnlyWhy says, for the trace, why a shared key was sealed for its routes
// only: the family already held, or the facade policy keeping it off a free one.
func routeOnlyWhy(familyTaken bool, policy sharedTierPolicy) string {
	if familyTaken {
		return "its wire family is served by another credential"
	}
	return "facade_default=" + string(policy.facade) + " keeps it off the anthropic wire's default"
}
