package cloudpublisher

import (
	"context"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
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
	// slotSealable is the adaptive-routing whitelist (ADR-121 §1, slice 3):
	// whether a credential SLOT may seal into the bundle — a slot is
	// sealable when the resolved pair_order names it for some harness ("a
	// run may occupy" the credential through any harness that can serve
	// it; the instance follows the tier walk). The fill and restore paths
	// ask it before sealing, so an unlisted slot is invisible to every
	// tier — consulted by no store read that a seal would depend on. nil
	// (no resolved policy — tests, unwired launches) admits everything:
	// today's behavior.
	slotSealable func(slot string) bool
	// refusedPinned is the resolved refused_pinned_key (#1999, ADR-121):
	// what happens to a shared-tier key refused or capped at launch whose
	// wire family another credential holds. "forfait" (the default) keeps
	// it out — the routes naming its provider spend the family's holder,
	// which for claw is the forfait billed as extra usage; "park" restores
	// it pinned-only and those routes park on the key's own refusal with a
	// durable retry.
	refusedPinned string
	// refusals is the record ADR-131 §5 owes: the slots this run's FROZEN
	// pair order refused for a credential the walk HELD, said once at the
	// walk's end. nil records nothing — the preview has no walk to report
	// (it names each exclusion per candidate, exclusionReason).
	refusals *pairOrderRefusals
	// runNative is the OR of every tier's native probe of ONE resolution —
	// the question `auto` asks since #1998: a Claude-native credential any
	// tier holds, open or closed, keeps every tier's facade key off the
	// wire's default, and the run parks on it. Built at the resolution root
	// beside the per-tier probes; nil asks nobody (a policy built without
	// tiers, as in unit tests, then behaves like `tier`).
	runNative *tierNative
	// ridesNoGateway is gatewayRoutes.sealable's negation for this run
	// (withGatewaySlot): no route of it may ride the openai_compatible
	// gateway, so no tier seals the gateway slot. The zero value admits it
	// (a policy built without the run's routing, as in unit tests).
	ridesNoGateway bool
}

// withGatewaySlot returns the policy with the run's gateway answer joined to
// the seal gate (gatewayRoutes.sealable): the one predicate every tier that
// could fill the gateway slot passes — the tenant, org and platform fills,
// the restore, the attached pool — and the preview reads.
func (pol sharedTierPolicy) withGatewaySlot(sealable bool) sharedTierPolicy {
	pol.ridesNoGateway = !sealable
	return pol
}

// gatewayOut reports a slot the run's routes never ride: the gateway's, on a
// run that rides no gateway route. Not a refusal of any frozen list — the run
// simply has no use for the credential.
func (pol sharedTierPolicy) gatewayOut(slot string) bool {
	return pol.ridesNoGateway && slot == slotOfProvider(secrets.ProviderOpenAICompatible)
}

// orNative composes two tiers' probes into the run's: held by either is held
// by the run. Each side keeps its own cache, so a question asked through the
// composite costs a tier at most one forfait list and one key list, whatever
// asks first. A nil side holds nothing.
func orNative(a, b *tierNative) *tierNative {
	return &tierNative{probe: func() bool { return a.holds() || b.holds() }}
}

func (p *Publisher) sharedTierPolicyFor(ctx context.Context) sharedTierPolicy {
	var rec *platformcfg.PlatformCredentials
	if p.platformAudience != nil {
		rec = p.platformAudience.Get(ctx)
	}
	return sharedTierPolicy{keysFirst: rec.PrefersKeys(), facade: rec.Facade(), refusedPinned: rec.RefusedPinned()}
}

// slotOfProvider is the wire spelling of an API key provider's policy
// slot: provider "anthropic" ↔ slot "anthropic_key".
func slotOfProvider(prov secrets.Provider) string { return string(prov) + "_key" }

// slotOfKind is the policy slot of an OAuth forfait kind: claude_code →
// claude_forfait, codex → chatgpt_forfait.
func slotOfKind(kind string) string {
	switch secrets.OAuthKind(kind) {
	case secrets.OAuthKindClaudeCode:
		return llmroute.CredClaudeForfait
	case secrets.OAuthKindCodex:
		return llmroute.CredChatGPTForfait
	}
	return kind
}

// wantForSlot is the INVERSE of the sealing maps above (slotOfKind /
// slotOfProvider), restricted to what a POOL may lend: the policy slot a
// fallback rung names → the pool credential that would fund it. Built FROM
// those tables by reversal — a hand-written switch would be a fourth copy of
// a vocabulary that already agrees three ways (llmroute's validated pairs,
// model.KnownPoolProviders, poolWantOrder) — and total over the pool's
// CLOSED vocabulary: an unmapped slot (the gateway's, which no pool want
// names — ADR-131 §5) refuses (ok=false) rather than inventing a want, and
// the derivation NAMES it in the door's log — a vocabulary extension that
// forgets the inverse is visible, never silent
// (TestWantForSlot_totalOverTheVocabulary pins the coverage).
func wantForSlot(slot string) (credpool.Credential, bool) {
	switch slot {
	case llmroute.CredClaudeForfait:
		return credpool.Credential{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}, true
	case llmroute.CredChatGPTForfait:
		return credpool.Credential{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindCodex)}, true
	}
	if prov, ok := strings.CutSuffix(slot, "_key"); ok {
		// The pool's vocabulary is the list poolWantOrder's keys come from —
		// the reversal stays inside one vocabulary.
		for _, known := range model.KnownPoolProviders {
			if secrets.Provider(prov) == known {
				return credpool.Credential{Source: credpool.SourceAPIKey, Ref: prov}, true
			}
		}
	}
	return credpool.Credential{}, false
}

// providerOfSlot is the provider hint whose credential the policy slot
// seals — the inverse of slotOfProvider/slotOfKind, built from the same
// vocabularies (allKnownProviders — the WALK's, the gateway included, so a
// providers allowlist naming openai_compatible admits its slot — and the
// forfait kinds' providers). "" for a slot no provider maps to.
func providerOfSlot(slot string) string {
	if prov, ok := strings.CutSuffix(slot, "_key"); ok {
		for _, known := range allKnownProviders {
			if secrets.Provider(prov) == known {
				return string(known)
			}
		}
	}
	switch slot {
	case llmroute.CredClaudeForfait:
		return string(secrets.ProviderAnthropic)
	case llmroute.CredChatGPTForfait:
		return string(secrets.ProviderOpenAI)
	}
	return ""
}

// withProviderAllowlist returns the policy with the tenant's provider
// allowlist (ADR-129 §5) joined to the seal gate: a slot whose provider the
// list excludes is sealed by NO tier, and a slot no provider maps to is
// not provably inside the list — refused, the same posture wantForSlot's
// refusal keeps. The walk pruned the provider, so no channel funds it.
func (pol sharedTierPolicy) withProviderAllowlist(allowed map[string]bool) sharedTierPolicy {
	if allowed == nil {
		return pol
	}
	inner := pol.slotSealable
	pol.slotSealable = func(slot string) bool {
		prov := providerOfSlot(slot)
		if prov == "" || !allowed[prov] {
			return false
		}
		if inner == nil {
			return true
		}
		return inner(slot)
	}
	return pol
}

// slotExcluder names WHICH of the run's frozen lists holds a credential slot
// out of the seal. The gate the walk seals through is their composition
// (withProviderAllowlist over withSlotWhitelist), so a message that
// prescribes a remedy cannot read its knob off that one boolean: it asks
// here, and the operator reads the list that actually holds the slot out
// (ADR-129 §7, ADR-131 §5).
type slotExcluder int

const (
	// excludedByProviderAllowlist: the tenant's providers allowlist refuses
	// the slot's provider, or the slot maps to no provider — not provably
	// inside the list, the posture withProviderAllowlist keeps. Named first,
	// because widening the pair order alone would not lift it.
	excludedByProviderAllowlist slotExcluder = iota
	// excludedByPairOrder: the providers allowlist admits the slot, so the
	// run's frozen routing pair order is what names no pair for it.
	excludedByPairOrder
)

// excluderOfSlot judges one slot, for the callers that have already
// established the composed gate refuses it: allowed is the run's frozen
// providers allowlist, nil when none is in force. A gate joined to the
// composition later extends THIS judgement with its own arm — one
// vocabulary, not a guess per message.
func excluderOfSlot(allowed map[string]bool, slot string) slotExcluder {
	if allowed != nil {
		if prov := providerOfSlot(slot); prov == "" || !allowed[prov] {
			return excludedByProviderAllowlist
		}
	}
	return excludedByPairOrder
}

// withSlotWhitelist returns the policy with the whitelist set, reporting the
// frozen list's refusals of HELD credentials into rec (ADR-131 §5). The one
// construction site for the gate: a nil admits everything (today's
// behavior for an unwired launch).
func (pol sharedTierPolicy) withSlotWhitelist(pairOrder []string, rec *pairOrderRefusals) sharedTierPolicy {
	pol.slotSealable = func(slot string) bool { return llmroute.SlotSealable(pairOrder, slot) }
	pol.refusals = rec
	return pol
}

// sealableSlot answers the whitelist for one credential slot, the run's
// gateway answer included (gatewayOut). A policy built without the gates
// admits everything.
func (pol sharedTierPolicy) sealableSlot(slot string) bool {
	if pol.gatewayOut(slot) {
		return false
	}
	return pol.listsAdmit(slot)
}

// listsAdmit answers the run's frozen lists alone — the routing pair order
// and the providers allowlist — without the gateway answer: whether a slot
// the run rides may seal at all.
func (pol sharedTierPolicy) listsAdmit(slot string) bool {
	return pol.slotSealable == nil || pol.slotSealable(slot)
}

// sealableHeldSlot is the seal gate for a credential the walk HOLDS: the
// same answer, plus the record ADR-131 §5 owes — a credential the walk
// resolved that the run's frozen pair order refuses is remembered, and the
// walk's end names it once. Every tier's fill and restore asks THIS one, the
// platform key leg included: which side of the resolve a tier's gate sits on
// decides whether its refusal can be named at all, so they all ask with the
// credential in hand. The key legs also ask it INSIDE their usability
// predicate (heldSlotUsable), one candidate earlier, where a refused slot
// answers before its meter is read. The pure sealableSlot is left to the sites that hold
// nothing to report (the door's pre-acquisition want) or whose refusal
// already has a line of its own: the pool grant, released with its reason,
// and the preview, which names every exclusion per candidate.
func (pol sharedTierPolicy) sealableHeldSlot(slot string) bool {
	// Asked first: a slot the run never rides is not a credential a frozen
	// list held out, and naming it would put the §5 line on every vendor run
	// of a team that holds a gateway key.
	if pol.gatewayOut(slot) {
		return false
	}
	if pol.sealableSlot(slot) {
		return true
	}
	pol.refusals.note(slot)
	return false
}

// heldSlotUsable joins the seal gate to a tier's key-usability predicate, so
// every key leg asks the frozen list with the CREDENTIAL IN HAND and BEFORE
// its meter: the candidate's slot reaches the §5 record, and a slot the list
// refuses answers unusable without the predicate's skip exit arming the run's
// retry clock (apiKeyUsable → skipTracker.note → credResolution's
// skippedReopensAt → store.Run.SkippedCredReopensAt, a durable write).
//
// A slot outside the run's routing is not a credential "passed over for a
// window": waking the run when it reopens cannot change the resolution — the
// resume replays the frozen list as stored, so the slot is refused again —
// while every arming spends one attempt of a bounded retry budget
// (pkg/runner's usageWindowRetryAt). The forfait legs already gate before
// their window read; this is the key legs' parity, and it costs less than the
// gate after the resolve: a refused slot's meter is never read at all.
//
// A PINNED key bypasses this predicate inside secrets.Resolve, as it bypasses
// the usability one: the operator named that key, so the pin still resolves
// and its fate is named by warnRefusedPins (a DROPPED pin, not an honoured
// one). The gate after the resolve is what keeps it out of the bundle.
func (pol sharedTierPolicy) heldSlotUsable(inner func(secrets.ApiKey) bool) func(secrets.ApiKey) bool {
	return func(k secrets.ApiKey) bool {
		if !pol.sealableHeldSlot(slotOfProvider(k.Provider)) {
			return false
		}
		return inner(k)
	}
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
// key may be the anthropic wire's default. Under `auto` the answer is the
// RUN's — every tier's facade stays off the default while any tier of the
// run holds a native credential; `tier` is the per-tier rule (capacity
// fall-through for the tier that itself holds nothing). `native` is only
// asked under `tier`, for a facade provider on a free family — once per
// resolution: at most one forfait list and one key list per tier.
func (pol sharedTierPolicy) facadeMayDefault(native *tierNative) bool {
	switch pol.facade {
	case platformcfg.FacadeAlways:
		return true
	case platformcfg.FacadeNever:
		return false
	case platformcfg.FacadeTier:
		return !native.holds()
	}
	return !pol.runNative.holds()
}

// isFacadeProvider names the providers that ride the anthropic wire without
// being Anthropic: they answer a claude id with their own model.
func isFacadeProvider(prov secrets.Provider) bool {
	return prov != secrets.ProviderAnthropic &&
		secrets.WireFamily(string(prov)) == secrets.WireFamily(string(secrets.ProviderAnthropic))
}

// restoreRefusedKey is the ONE judge — fill, restore and preview ask it —
// of whether the restore keeps a refused shared-tier key OUT of the bundle
// under the policy: refused_pinned_key=forfait leaves a refused key out of
// a family another credential holds, and the routes naming its provider
// spend that holder (claw bills the forfait as extra usage) instead of
// parking on the key's own refusal. The facade carve-out travels with the
// knob (ADR-121 § Arbitrated 1): a facade key (z.ai, Moonshot) has no
// forfait alternative, so it is out of the knob's scope ENTIRELY — it
// always comes back, in either pinned-only shape.
func (pol sharedTierPolicy) restoreRefusedKey(prov secrets.Provider, familyTaken bool) bool {
	return pol.refusedPinned == llmroute.RefusedPinnedForfait &&
		familyTaken &&
		!isFacadeProvider(prov)
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
func fillAPIKeySlotAs(bundle *secrets.RunBundle, taken map[string]bool, prov secrets.Provider, plaintext, baseURL, fingerprint string, outcome sealOutcome) (pinnedOnly bool) {
	if outcome == sealPinnedOnly {
		setAPIKey(bundle, prov, plaintext, baseURL, fingerprint, true)
		return true
	}
	return fillAPIKeySlot(bundle, taken, prov, plaintext, baseURL, fingerprint)
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

// newTierNative builds the probe for the tier whose forfaits live under any
// of forfaitOwners and whose keys live under keyScope. A store that cannot
// answer counts as holding NONE — the platform tier's rule for a degraded
// read: the tier a deployment runs on must not lose its only credential to a
// store blip. It is said out loud, since a facade may then take the default.
func (p *Publisher) newTierNative(ctx context.Context, tier, keyScope, botID string, forfaitOwners ...string) *tierNative {
	return &tierNative{probe: func() bool {
		if p.oauthForfait != nil {
			for _, forfaitOwner := range forfaitOwners {
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
		}
		if p.apiKeys != nil {
			visible, err := p.apiKeys.ListByTeam(store.WithTenant(ctx, keyScope), keyScope, "")
			if err != nil {
				p.logger.Warn("cloudpublisher: %s tier key list for the facade policy: %v — read as holding none: a facade key may take the anthropic wire's default for this launch", tier, err)
				return false
			}
			// Both audiences, as Resolve reads them: a key the walk will
			// withhold for this launch's bot OR repository is not a native
			// credential the run holds, and counting it would keep every
			// facade key off the anthropic wire's default for a run that
			// then seals nothing.
			repo := secrets.LaunchRepoFromContext(ctx)
			for _, k := range visible {
				if k.Provider == secrets.ProviderAnthropic && k.ServesBotForLaunch(botID) && k.ServesRepoForLaunch(repo) {
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

// llmCredentialOwed reports whether the walk ends owing this run an LLM
// credential it did not get: the bundle holds none, the workflow calls a
// model (a nil one is unknown — read as yes, since a run refused the
// credential it needed is the failure that matters), and the run is not
// env-funded (its routes ride the runner's gateway and consult no tier).
//
// ONE judgement, read by the "no credential resolved" warning and by ADR-131
// §5's line — which is that warning's only explanation — so the two cannot
// disagree about whether the run was left unfunded.
func llmCredentialOwed(bundle secrets.RunBundle, wf *ir.Workflow, envFunded bool) bool {
	return !envFunded && !holdsLLMCredential(bundle) && (wf == nil || wf.UsesLLM())
}

// holdsDefaultLLMCredential reports whether a route the walk did not
// attribute — an UNPINNED route — has something of the run's own to spend:
// a forfait, or a default-channel key of any slot but the gateway's. A
// pinned-only key serves the routes that name its provider and nobody else;
// a gateway key serves the routes the walk attributes to it and never an
// unattributed one (ADR-131 §5: the gateway slot never pays an unattributed
// route). ONE predicate for every reader of the question, on the sealed
// bundle here and on the preview's predicted fills (defaultLLMCredentialIn):
// the pool tier's admission, the legacy pool's gate, the fallback door, and
// the sovereign refusal of a run nothing of the team's own funds.
func holdsDefaultLLMCredential(b secrets.RunBundle) bool {
	slots := make([]secrets.Provider, 0, len(b.APIKeys))
	for prov, key := range b.APIKeys {
		if key != "" {
			slots = append(slots, prov)
		}
	}
	return defaultLLMCredentialIn(slots, len(b.OAuthCredentials))
}

// defaultLLMCredentialIn is holdsDefaultLLMCredential over the two channels
// as a caller sees them: the default-channel key slots and the forfait count.
func defaultLLMCredentialIn(defaultKeySlots []secrets.Provider, oauthKinds int) bool {
	if oauthKinds > 0 {
		return true
	}
	for _, prov := range defaultKeySlots {
		if prov != secrets.ProviderOpenAICompatible {
			return true
		}
	}
	return false
}

// familyHolderName names, for the audit line, what holds a refused key's
// wire family in the bundle — the credential the routes naming its provider
// will spend instead. Sorted, because a map's "first hit" is whatever Go
// feels like today: the fact travels with the log, not with iteration
// order.
func familyHolderName(bundle *secrets.RunBundle, prov secrets.Provider) string {
	fam := secrets.WireFamily(string(prov))
	var holders []string
	for kind := range bundle.OAuthCredentials {
		if secrets.WireFamily(kind) == fam {
			holders = append(holders, "the "+kind+" forfait")
		}
	}
	for p := range bundle.APIKeys {
		if p != prov && secrets.WireFamily(string(p)) == fam {
			holders = append(holders, "the "+string(p)+" key")
		}
	}
	sort.Strings(holders)
	if len(holders) == 0 {
		return "the credential holding the " + fam + " wire"
	}
	return strings.Join(holders, " and ")
}

// routeOnlyWhy says, for the trace, why a shared key was sealed for its routes
// only: the family already held, or the facade policy keeping it off a free one.
func routeOnlyWhy(familyTaken bool, policy sharedTierPolicy) string {
	if familyTaken {
		return "its wire family is served by another credential"
	}
	return "facade_default=" + string(policy.facade) + " keeps it off the anthropic wire's default"
}
