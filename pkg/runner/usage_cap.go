package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The operator's subscription cap, runner side.
//
// Two jobs live here. The first is PUBLISHING what a run measures: every
// pod sees only its own session, so without a shared record each of them
// rediscovers the ceiling by spending against it. The second is the
// PRE-FLIGHT: a claimed run asks what the fleet already knows before it
// clones a repo or starts a container, and parks for free when the answer
// is "no headroom".
//
// Parking reuses the provider-refusal path wholesale (usage_retry.go): the
// run is marked failed_resumable with the cap as its error, a durable retry
// is armed for the instant the window reopens, and the delivery is acked.
// The operator's ceiling therefore inherits, for free, the one property
// that matters — a capped run is not lost, it is waiting.

// runCredKeys is the per-run credential identity the meter draws on: the
// scope (tenant-own vs platform) plus one fingerprint per credential SHAPE
// the bundle holds. A reading is keyed by the shape the node actually
// exercised — the delegate stamps its provider-routing label on each
// Reading (usagecap.Reading.Source) — because a bundle may carry both a
// z.ai token and an Anthropic key, and a node pinned `provider: anthropic`
// spends the Anthropic key while the bundle-default precedence points at
// z.ai. Charging that refusal to the z.ai fingerprint would make the
// evidence-based skip park the healthy key and keep the frozen one.
type runCredKeys struct {
	scope       string
	zaiFP       string
	moonshotFP  string
	anthropicFP string
	oauthFP     string
	// gatewayFP is the run's openai_compatible credential's fingerprint —
	// the server's URL-folded stamp, carried by the bundle (PR-B'1). ""
	// when the run holds no gateway credential.
	gatewayFP string
	// pinnedSlots are the slots funded ONLY by a key a shared tier filled
	// because a route pins that provider (secrets.Credentials.PinnedAPIKeys).
	// Their fingerprint is real — a reading that NAMES such a slot is charged
	// to it, which is the whole point of stamping one — but they are not the
	// run's default credential: no unpinned node can spend them, so an
	// unattributable reading must not land there either.
	pinnedSlots map[string]bool
	// pinnedScopes is the meter scope a PINNED slot's own readings belong to,
	// when that differs from the run's.
	//
	// A pinned key exists only beside another credential on its wire, and
	// when that other one is the TENANT's the run scope is the tenant's
	// private ledger — which is the wrong ledger for an org's or the
	// platform's shared key: a window refusal one borrower measures would
	// reach no other borrower of the same account, the inversion this
	// struct's scope exists to prevent. So the owner's scope travels with the
	// slot instead of being re-derived from the run.
	pinnedScopes map[string]string
	// ambientFP is the fingerprint of the credential the pod inherits, read
	// host-side once (delegate.AmbientCredentialFingerprint — the claude CLI's
	// reading of the pod). "" when the pod carries none — its "Not logged in"
	// a refusal — a cloud-provider switch answers instead, or the run refuses
	// the pod env (ambientRefused). It is the meter key of the nothing-held
	// default, not a slot of the run: no label of the run resolves to it, only
	// a session with no credential of its own spends it. A claw call reads its
	// own channels instead (clawUnslottedKey).
	ambientFP string
	// ambientRefused is the stamp (secrets.Credentials.AmbientRefused): every
	// fallback of the run to the pod env answers a typed refusal, so its
	// nothing-held default stays on the no-credential meter and a claw walk
	// into the pod env meters nothing (clawUnslottedKey).
	ambientRefused bool
	// defaultHeld: the run holds a default credential on the anthropic wire —
	// a key of the run's own, a forfait dir — fingerprinted or not. A legacy
	// OAuth record carries no fingerprint (firstHeld cannot name it) and is
	// still what the run's sessions spend: its default is that slot's
	// fingerprint-less meter, never the pod's ambient account.
	defaultHeld bool
	// sandboxed is the run's sandbox as the pre-flight reads it
	// (usageCapPreflight: the mode the floored workflow resolves — every node
	// shares the run's sandbox): a claw node then resolves in the container,
	// over the env the crossing hands it, and a pi node reads the container's
	// agent dir. The recording side never reads it: it keys the labels
	// sessions stamp.
	sandboxed bool
}

// scopeFor returns the meter scope a reading on this slot belongs to: the
// run's own, except a slot funded by a shared tier's pinned key, whose
// readings belong to the ledger of whoever owns that key.
func (k runCredKeys) scopeFor(slot string) string {
	if s := k.pinnedScopes[slot]; s != "" {
		return s
	}
	return k.scope
}

// bySlot returns the fingerprint held for one anthropic-wire slot, "" when
// the run carries none. The switch is the only place slot names meet struct
// fields, so the walks below can be written against
// secrets.AnthropicWireSlotOrder rather than restating the precedence.
func (k runCredKeys) bySlot(slot string) string {
	switch slot {
	case string(secrets.ProviderZAI):
		return k.zaiFP
	case string(secrets.ProviderMoonshot):
		return k.moonshotFP
	case string(secrets.ProviderAnthropic):
		return k.anthropicFP
	case string(secrets.OAuthKindClaudeCode):
		return k.oauthFP
	case string(secrets.ProviderOpenAICompatible):
		return k.gatewayFP
	}
	return ""
}

// usageCapCredKeys reads the run's resolved credentials once. Scope: a
// bundle carrying any credential the TENANT resolved is the tenant's own;
// anything else shares a cross-tenant meter — a slot the publisher filled
// from the DB-backed platform tier (the deployment's single subscription),
// one the credential POOL filled with a contributor's lent one (the
// donor's single subscription, borrowed by several tenants in turn), and
// one the ORG tier filled with the org's own key (one subscription serving
// every team of its audience). All three ride the bundle exactly like a
// tenant credential and none is one; metering a shared credential per
// borrower would open one ledger per borrower of the SAME account, so what
// one of them measured — a refusal, a window at 95% — would reach none of
// the others.
//
// The org tier gets its OWN scope rather than the platform one: its
// subscription is the org's, and merging it with the deployment's would
// make one org's exhausted window park every other tenant's runs.
func usageCapCredKeys(ctx context.Context, msg *queue.RunMessage) runCredKeys {
	k := runCredKeys{scope: usagecap.ScopePlatform}
	creds, ok := secrets.CredentialsFromContext(ctx)
	// The pod's ambient credential exists whether or not the run carries a
	// bundle, and a stamped run refuses it: its nothing-held default stays on
	// the no-credential meter. Read host-side, once, beside the bundle's own
	// facts.
	k.ambientRefused = ok && creds.AmbientRefused
	if !k.ambientRefused {
		k.ambientFP = delegate.AmbientCredentialFingerprint()
	}
	if !ok {
		return k
	}
	held := func(slot string, present bool) (tenant, org bool) {
		if !present {
			return false, false
		}
		return creds.IsTenantOwned(slot), creds.IsOrgSourced(slot)
	}
	// Every slot of the wire, in one walk: a scope decided from a subset
	// would meter a run holding only the missing provider's key on the
	// cross-tenant ledger, mixing its readings with every other borrower's.
	//
	// The run's scope is its DEFAULT credentials' — what an unattributed
	// reading and a default-precedence session are charged to. A pinned key
	// carries its owner's ledger itself (pinnedScopes below), and letting it
	// decide the run's scope would meter the default credential beside it —
	// the platform's forfait next to an org's pinned z.ai key — on a ledger
	// its owner's other borrowers never read. Only a run holding pinned keys
	// alone takes its scope from them.
	scopeOf := func(pinned bool) (anyTenant, anyOrg, anyHeld bool) {
		for _, slot := range secrets.AnthropicWireSlotOrder {
			present := creds.APIKey(secrets.Provider(slot)) != ""
			if pinned {
				present = creds.IsPinnedSlot(slot)
			}
			if secrets.OAuthKind(slot).Valid() {
				present = !pinned && creds.OAuthDir(slot) != ""
			}
			tenant, org := held(slot, present)
			anyTenant = anyTenant || tenant
			anyOrg = anyOrg || org
			anyHeld = anyHeld || present
		}
		return anyTenant, anyOrg, anyHeld
	}
	anyTenant, anyOrg, anyDefault := scopeOf(false)
	k.defaultHeld = anyDefault
	if !anyDefault {
		anyTenant, anyOrg, _ = scopeOf(true)
	}
	switch {
	case anyTenant:
		k.scope = usagecap.TenantScope(msg.TenantID)
	case anyOrg:
		k.scope = usagecap.OrgScope(msg.OrgID)
	}
	k.zaiFP = creds.Fingerprint(string(secrets.ProviderZAI))
	k.moonshotFP = creds.Fingerprint(string(secrets.ProviderMoonshot))
	k.anthropicFP = creds.Fingerprint(string(secrets.ProviderAnthropic))
	k.oauthFP = creds.Fingerprint(delegate.BackendClaudeCode)
	// The gateway slot: the server's URL-folded stamp travels in the bundle
	// (PR-B'1) and the runner stamps it verbatim — its identity cannot be
	// recomputed from the secret alone.
	k.gatewayFP = creds.Fingerprint(string(secrets.ProviderOpenAICompatible))
	for _, slot := range secrets.AnthropicWireSlotOrder {
		if !creds.IsPinnedSlot(slot) {
			continue
		}
		if k.pinnedSlots == nil {
			k.pinnedSlots = map[string]bool{}
			k.pinnedScopes = map[string]string{}
		}
		k.pinnedSlots[slot] = true
		// Its owner's ledger, by the same rule the run scope follows: the
		// platform's single account is one meter for every tenant it serves,
		// an org's is one for every team of its audience.
		switch {
		case creds.IsPlatformSourced(slot), creds.IsPoolSourced(slot):
			k.pinnedScopes[slot] = usagecap.ScopePlatform
		case creds.IsOrgSourced(slot):
			k.pinnedScopes[slot] = usagecap.OrgScope(msg.OrgID)
		}
	}
	return k
}

// forSource keys a reading under the credential its session actually ran
// on. The source labels are providerFingerprint's vocabulary: a facade URL
// is a facade token, "anthropic-direct" the Anthropic API key,
// "anthropic-oauth" the OAuth dir. An empty label (older binary) falls back
// to the run's default (defaultKey): the bundle-default precedence,
// secrets.AnthropicWireSlotOrder — anthropicCredEnvForCLI's contract, read
// from the list it is written against — and on a run holding none of it the
// meter of the session that spends the pod's env, the one the pre-flight
// reads for that same default. A rotated token therefore opens a fresh meter
// instead of inheriting the readings of the account it replaced.
//
// An ambient-credential label ("anthropic-env:<fingerprint>") is the POD's
// inherited key — or a node's own key on top of it — and it is ONE account
// for the whole deployment: keyed on the PLATFORM scope under the
// fingerprint the delegate computed host-side (AmbientSourceFingerprint
// reads it back), so what one tenant's run measures reaches every other
// run that would spend the same key, and a node key never shares a meter
// with the pod key it sits beside. Keying it on the run's scope would
// split one account into one ledger per borrower — the split
// usageCapCredKeys exists to prevent for shared credentials.
//
// Bare "anthropic-env" is the session whose spend no inherited credential
// names: a pod with no ambient auth at all — its "Not logged in" is a
// refusal — or one whose cloud-provider switch answers instead, where the
// spend is that cloud account's and no ambient variable names it. It is
// keyed on the scope with no credential, never on the bundle default:
// charged to the head of the precedence, that refusal would bench a healthy
// z.ai key the session never touched.
//
// Every facade renders as "facade:<slot>:<base-url>", and the SLOT is what
// names the vendor now that two ride this wire — the delegate stamps it on the
// env it built (AnthropicWireFacadeSlot reads it back). Charging a Moonshot
// refusal to the z.ai fingerprint would park the healthy key and keep the
// frozen one — the failure this whole struct exists to avoid.
//
// A label that NAMES a slot is answered by that slot ALONE — a facade, the
// Anthropic key, the OAuth dir: when the run holds no fingerprint for it, the
// reading is keyed on the scope with no credential rather than on whichever
// key the precedence happens to start with. That
// state is reachable — a pod-level MOONSHOT_API_KEY funds a moonshot-pinned
// node (facadeCredEnvForHint) without being a BYOK record, so the run carries
// no moonshot fingerprint while its readings still say moonshot — and the
// neighbour it would otherwise charge is a healthy key the wall never touched.
//
// An UNRECOGNISED facade label (an operator base URL forwarded from the
// ambient env, a label from a binary that predates the stamp) names no slot at
// all, and there the run's default is the only answer available, as for an
// empty one.
func (k runCredKeys) forSource(source string) string {
	if fp, ok := delegate.AmbientSourceFingerprint(source); ok {
		return usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, fp)
	}
	if slot, named := k.slotForSource(source); named {
		return k.keyForSlot(slot)
	}
	return k.defaultKey()
}

// slotForSource names the slot a reading's source label is charged to — ""
// when the route it names ran on no bundle credential — by the rules
// forSource documents, and named=false for a label that names no route at
// all (an empty one, an unrecognised one), which the run's default answers.
// The whole "anthropic-env" family is named: bare for a session whose spend
// no inherited credential names, fingerprinted labels being answered by
// forSource before this walk, and a fingerprint-less member (a hand-written
// label, a future shape) charging nobody rather than the run's default.
func (k runCredKeys) slotForSource(source string) (slot string, named bool) {
	switch {
	case strings.HasPrefix(source, "facade:"):
		if s := delegate.AnthropicWireFacadeSlot(source); s != "" {
			return s, true
		}
	case source == "anthropic-direct":
		if k.anthropicFP != "" {
			return string(secrets.ProviderAnthropic), true
		}
		return "", true
	case source == "anthropic-oauth":
		if k.oauthFP != "" {
			return string(secrets.OAuthKindClaudeCode), true
		}
		return "", true
	case delegate.IsAmbientSource(source):
		return "", true
	}
	return "", false
}

// keyForSlot is the meter key the readings of one credential slot land on:
// the slot's own ledger scope — a shared tier's pinned key is metered on its
// owner's ledger even when the run itself is tenant-scoped — and its
// fingerprint. "" is the run's scope with no credential, where a session
// whose spend no credential names records (the bare "anthropic-env").
func (k runCredKeys) keyForSlot(slot string) string {
	return usagecap.Key(delegate.BackendClaudeCode, k.scopeFor(slot), k.bySlot(slot))
}

// routeKey is the meter key a primary anthropic-wire route draws on, and
// whether the operator's cap meters that route at all.
//
// A claude_code route asks the delegate itself which source its session will
// stamp (delegate.AnthropicRouteSource, under the hint the executor will hand
// it), so the pre-flight reads the ledger the session will write — a forfait,
// the run's own key, a key pinned for the route or the pod's ambient env. A
// route the delegate refuses before spawning (a facade hint with no key)
// spends nothing and is not metered. A pi route on its Anthropic provider asks
// the delegate the same question (delegate.PiAnthropicRouteSource: pi's own
// credential order, not claude_code's channel set, and the container's agent
// dir on a sandboxed run); on any other provider pi's hint names the provider
// it spends, over the model's prefix; kimi and grok spend their own config and
// are not metered; other backends route on the spec, as the spend ledger books
// them (credentialSlotForRoute). A slot off the anthropic wire is not what the
// cap meters. A nothing-held answer is metered only on a claw anthropic-wire
// spec, read off claw's own resolution (clawUnslottedKey): the run's Anthropic
// key where claw still reaches it past the slots the ledger's walk names — a
// GLM id with no z.ai key, in process — else the account the request carries,
// by the channels claw's client reads, where a wall this cap keeps can stand
// in its way at all. Every other nothing-held shape funds elsewhere or is
// unreadable — opencode resolves its own config, a gateway route the
// deployment's gateway pair, a vendor id its own pod env — and the reading
// side books every one of those on nobody, so keying it here would park the
// run on evidence no spend of its can raise. A route the walk cannot read
// keeps the run's default credential — defaultKey, the ledger the coming
// session writes.
func (k runCredKeys) routeKey(ctx context.Context, route model.WireRoute) (key string, metered bool) {
	if !route.Readable {
		return k.defaultKey(), true
	}
	hint := model.RouteProviderHint(ctx, route.Backend, route.Hint, route.Model)
	if route.Backend == delegate.BackendClaudeCode {
		source, refused := delegate.AnthropicRouteSource(ctx, hint)
		if refused {
			return "", false
		}
		return k.forSource(source), true
	}
	switch route.Backend {
	case delegate.BackendKimi, delegate.BackendGrok:
		// Vendor CLIs paid by their own config: no credential of the run,
		// and none of the pod's anthropic-wire ones.
		return "", false
	case delegate.BackendPi:
		// pi on its Anthropic provider resolves its own credential order — its
		// store, then its bearer, OAuth and API-key channels — so the delegate
		// names the source the session will stamp, exactly as it does for
		// claude_code above, and the pre-flight reads the meter that label
		// keys: the run's key, an inherited account by its fingerprint, the
		// bare meter, or nothing iterion keeps. pi reads no ambient stamp: a
		// stamped run's pi node spends what pi resolves, the pod's channels
		// included, and this names that account.
		if source, metered, anthropic := delegate.PiAnthropicRouteSource(ctx, route.Model, hint, k.sandboxed); anthropic {
			if !metered {
				return "", false
			}
			return k.forSource(source), true
		}
	}
	creds, _ := secrets.CredentialsFromContext(ctx)
	var slot string
	if h := strings.ToLower(strings.TrimSpace(hint)); route.Backend == delegate.BackendPi && h != "" {
		if secrets.WireFamily(h) != secrets.WireFamilyAnthropic {
			return "", false
		}
		slot = heldForRoute(creds, h)
	} else {
		slot = credentialSlotForRoute(creds, route.Backend, route.Model)
	}
	if slot != "" && secrets.WireFamily(slot) != secrets.WireFamilyAnthropic {
		return "", false
	}
	if slot == "" {
		// No slot the ledger's walk names funds the route. A claw spec on the
		// anthropic wire still resolves — through the run's Anthropic key in
		// process, through the pod env otherwise (inherited in process, the
		// crossing's in the container) — and is read off claw's own walk; a
		// pi route on its Anthropic provider was answered above, and pi's
		// other providers, like every other backend, fund from config iterion
		// does not read: an account whose walls never land on this meter. Not
		// metered.
		if route.Backend == delegate.BackendClaw && wireForRoute(route.Backend, route.Model) == anthropicWire {
			return k.clawUnslottedKey(creds, route.Model, hint, k.sandboxed)
		}
		return "", false
	}
	return k.keyForSlot(slot), true
}

// clawUnslottedKey is the meter key of a claw call on the anthropic wire that
// no slot its caller's walk named funds — a claw node's route (in the
// container on a sandboxed run), a supervisor's spec (always in process) —
// and whether the cap meters it at all. One place for both callers, in claw's
// own order.
//
// The run's Anthropic key comes first where claw's resolution still reaches
// it. In process, ResolveWithContext spends it on every anthropic spec its
// earlier branches pass by (the keyed factory, APIKeyForRoute) — a GLM id
// with no z.ai key of the run included, which both callers' walks answer with
// the z.ai slot alone — at the destination model.RunCredentialBaseURL names;
// a GLM id bound for the real Anthropic wire there (model.GLMUnservedAt)
// meets no wall and is not metered. In the container the crossing carries
// that key only for a node whose spec pins anthropic, which a GLM spec does
// not (forwardableProviderEnv pins it on zai), and such a route named the
// slot already.
//
// Past it, the pod env, read off claw's own resolution under the hint it
// resolves with (model.AmbientEnvMeter): the account the request carries, by
// the channels claw's client reads. Never the claude CLI's reading of the pod
// (ambientFP): claw reads no CLAUDE_CODE_OAUTH_TOKEN and sends a key alone
// where a bearer sits beside it, so that reading names an account claw does
// not spend.
//
// An account keys the platform meter of its fingerprint, the one every lane's
// label lands on wherever that lane sends the same set. A request carrying
// none of the env's credentials — this host's Claude Code login read from
// disk, or none at all — keys the scope's no-credential meter. Not metered
// where no wall this cap keeps stands in the call's way: a walk claw refuses
// before any client exists (no client, no account — the typed refusal is the
// answer and the route's own fallbacks fire), which is every walk of a
// stamped run past its own key (secrets.Credentials.AmbientRefused): the
// registry answers ErrAmbientCredentialRefused in process, and the crossing
// does for the container — it carries none of the pod's credentials and
// refuses a node nothing of the run funds before any runner starts
// (model.ClawSandboxCredential reads that crossing) — z.ai's synthesis, a GLM
// id no destination of the env serves.
func (k runCredKeys) clawUnslottedKey(creds secrets.Credentials, spec, hint string, sandboxed bool) (string, bool) {
	if !sandboxed {
		if slot := heldForRoute(creds, string(secrets.ProviderAnthropic)); slot != "" {
			if model.GLMUnservedAt(spec, model.RunCredentialBaseURL(spec, hint)) {
				return "", false
			}
			return k.keyForSlot(slot), true
		}
	}
	// The stamp refuses the pod env, never the run's own key: its walk past
	// the arm above is refused, the one before it spends.
	if k.ambientRefused {
		return "", false
	}
	fp, metered := model.AmbientEnvMeter(spec, hint, sandboxed)
	if !metered {
		return "", false
	}
	if fp == "" {
		return k.keyForSlot(""), true
	}
	return usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, fp), true
}

// firstHeld is the bundle-default precedence: the first slot of
// secrets.AnthropicWireSlotOrder the run actually carries.
func (k runCredKeys) firstHeld() (fp, slot string) {
	for _, slot := range secrets.AnthropicWireSlotOrder {
		if k.pinnedSlots[slot] {
			// Funded for the routes that NAME it and for nothing else. The
			// default precedence in the delegate cannot reach it, so a
			// reading with no attributable source cannot have been spent on
			// it — charging it here would park a key this run's unpinned
			// work never touched.
			continue
		}
		if fp := k.bySlot(slot); fp != "" {
			return fp, slot
		}
	}
	return "", ""
}

// defaultKey is the meter of a spend nothing attributes — a reading whose
// label names no route (forSource) and a route the pre-flight cannot read
// (routeKey), one meter for both sides: the run's default credential when the
// bundle holds one — its fingerprint's meter, the scope's fingerprint-less
// one for a legacy record that carries none — and when it holds none, the
// ledger the coming session writes — the pod's ambient account's meter where
// the pod carries the credential (nothingHeldKey), the bare no-credential
// meter where it does not. Keying the nothing-held arm on the bare meter
// would read a ledger no ambient wall ever lands on: the account's refusals
// travel under its fingerprinted label.
func (k runCredKeys) defaultKey() string {
	if _, slot := k.firstHeld(); slot != "" {
		return k.keyForSlot(slot)
	}
	if k.defaultHeld {
		return k.keyForSlot("")
	}
	return k.nothingHeldKey()
}

// nothingHeldKey is the meter key of a session with no credential of the run:
// the pod's ambient account's meter where the pod carries the credential —
// the same platform-scope key the account's fp'd ambient labels key on — and
// the scope's no-credential meter otherwise ("Not logged in" refusals, a
// cloud-provider switch's spend, a stamped run's refused fallbacks).
func (k runCredKeys) nothingHeldKey() string {
	if k.ambientFP != "" {
		return usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, k.ambientFP)
	}
	return k.keyForSlot("")
}

// usageCapKey is the run's DEFAULT credential key (runCredKeys.defaultKey) —
// the one a session with no source label is charged to (forSource), and what
// the pre-flight reads for a route it cannot read (runCredKeys.routeKey).
func usageCapKey(ctx context.Context, msg *queue.RunMessage) string {
	return usageCapCredKeys(ctx, msg).defaultKey()
}

// usageGuardFor builds the guard for one run: the machine-wide policy
// SOURCE, with every reading published to the shared store under the run's
// credential key. The guard re-reads the source per evaluation, so a cap
// tightened at runtime (the DB-backed settings record) bites a run already
// in flight — which is also why a LIVE source that answers "nothing capped
// right now" still gets a guard: the answer can change before the run
// ends.
//
// RECORDING IS NOT ENFORCING, and the guard is the only path either
// travels. A deployment that configured no cap still needs the provider's
// refusals on the shared ledger: the credential-tier skips (a frequency
// refusal, a rejected credential) read nothing else, and route the next
// run around a credential the provider will not serve. Gating the guard on
// the cap policy made every one of them inert on exactly the deployments
// that never asked for a ceiling. So a ledger alone is reason enough; only
// with neither a policy source nor a store — nothing to enforce, nobody to
// tell — is there no guard.
func (r *Runner) usageGuardFor(ctx context.Context, msg *queue.RunMessage, logger *iterlog.Logger) *usagecap.Guard {
	src := r.cfg.UsageCapSource
	store := r.cfg.UsageCaps
	// A static policy with no cap can never block; a live source can start
	// blocking mid-run, so it always counts as enforceable.
	enforceable := src != nil
	if pol, static := src.(usagecap.StaticPolicy); static && !usagecap.Policy(pol).Enabled() {
		enforceable = false
	}
	if !enforceable && store == nil {
		return nil
	}
	if src == nil {
		// Inert policy: the guard observes and publishes, and blocks nothing.
		src = usagecap.StaticPolicy(usagecap.Policy{})
	}
	keys := usageCapCredKeys(ctx, msg)
	return usagecap.NewGuardWithSource(src, func(reading usagecap.Reading) {
		if store == nil {
			return
		}
		// Keyed per reading, not per run: the session's provider-routing
		// label names the credential the node actually spent, and a run
		// whose nodes pin different providers must not charge one key's
		// refusal to another's meter.
		key := keys.forSource(reading.Source)
		// Detached: the reading that stops a run arrives exactly as that
		// run's context is about to be cancelled, and it is the single
		// most valuable thing to publish.
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageCapStoreTimeout)
		defer cancel()
		if err := store.Record(wctx, key, reading); err != nil && logger != nil {
			// Best effort by design: an unpublished reading costs the next
			// pod a wasted call, never this run its correctness.
			logger.Warn("runner: publish usage reading (%s): %v", key, err)
		}
	})
}

// usageCapStoreTimeout bounds the shared-store round trips. Short: a wedged
// store must never hold a run's stream goroutine, and both callers degrade
// safely (publish is best effort, pre-flight fails open).
const usageCapStoreTimeout = 5 * time.Second

// usageCapPreflight reports the error a claimed run should fail with when
// the operator's cap leaves no headroom, or nil to proceed.
//
// It FAILS OPEN on every uncertainty — no policy, no store, an unreadable
// store, nothing measured yet, a reading whose window has since rolled over.
// A cap exists to protect a subscription from a fleet of bots, not to strand
// the fleet when its bookkeeping is unavailable: the mid-run guard is still
// armed behind it, so the worst case of failing open is one wasted call.
//
// It runs at ADMISSION, where the run has no logger of its own: loop.go
// registers the run log writer after this call has decided the run may pay for
// a workspace at all, so every line below lands on the runner's log and none
// in the `run.log` the studio reads. One fork carries the run id as a FIELD
// through the whole walk — the shape loop_nats.go's admission-rejection lines
// use, and the chokepoint the lines that do NOT format msg.RunID themselves
// need: the two a supervisor drop writes (runview.PoolSurvivingSpecs,
// supervise.SpecsFromWorkflow name the supervisor) and the credential-read
// refusal in cappedRoutes (it names the credential key). Rendered by the
// runner's own format, which is JSON (cmd/iterion/runner.go).
func (r *Runner) usageCapPreflight(ctx context.Context, wf *ir.Workflow, msg *queue.RunMessage, logger *iterlog.Logger) error {
	if r.cfg.UsageCapSource == nil || r.cfg.UsageCaps == nil {
		return nil
	}
	logger = logger.WithField("run_id", msg.RunID) // nil-safe: a nil fork stays nil
	// The LIVE effective policy — env defaults + the runtime settings
	// record, one TTL-bounded lookup per claimed run.
	pol := r.cfg.UsageCapSource.Effective(ctx)
	if !pol.Enabled() {
		return nil
	}
	// Refuse in advance only what could not possibly avoid spending. A
	// workflow with any model-free path — the collect half of a two-mode
	// feed bot, say — is let through; under a HARD cap the MID-RUN guard
	// stops it at a claude_code call, while a soft cap, which stops nothing in
	// flight, or a backend that reports no readings (claw, pi until a refusal)
	// lets a run that takes its model path spend to the end. Accepted on
	// purpose: blocking it here loses what it was there to do — for a
	// collector, material no later run recovers, since a feed serves a short
	// window and does not remember what nobody fetched.
	if !wf.AlwaysReachesLLM() {
		if logger != nil {
			logger.Debug("runner: run %s makes no model call — usage cap not applied", msg.RunID)
		}
		return nil
	}
	// The cap meters the Anthropic wire — its readings come from the
	// claude_code delegate and nowhere else, keyed by the credential each
	// session spent (runCredKeys.forSource). A run whose every route is
	// pinned off that wire (claw/openai, codex) can
	// never spend what the cap protects: parking it for the anthropic
	// weekly reset strands it for nothing, which is how a fully pinned
	// two-node rite froze for five days while its single-node sibling
	// sailed through (#668). Read under the launch's own overrides, on
	// PRIMARY routes only — a rescue `fallbacks:` route onto the wire
	// fires on a failure the mid-run guard already refuses, and cannot
	// justify refusing the run before it starts; the view's RunFallbacks
	// stay zero for the same rule, and the walk reads no fallback chain.
	// Every uncertainty answers "reachable". A supervisor the run will
	// spawn is a route too — one every execution takes (supervisorRouteID).
	launchVars, haveLaunch := r.launchReading(ctx, msg, logger)
	view := model.RouteView{Workflow: wf, Overrides: modelOverridesFromMsg(msg.ModelOverrides), AllowedProviders: msg.LLMProviders}
	if haveLaunch {
		view = model.RouteViewForDispatch(wf, modelOverridesFromMsg(msg.ModelOverrides), nil, launchVars)
		view.AllowedProviders = msg.LLMProviders
	}
	routes := model.AnthropicWireRoutes(view)
	// One reading of the run's own topology, shared by the supervisor walk
	// and the park decision: a transition this launch decided against is on
	// no execution of this run.
	open := launchVarEdges(wf, launchVars, haveLaunch)
	sups := preflightSupervisors(ctx, wf, msg, view.DecidedVars(), open, logger)
	// The adaptive-routing ladder's escapes (ADR-121 §1): a node whose
	// PRIMARY rides the capped wire but whose computed ladder carries an
	// open stage OFF it is not park-forcing — the capped call fails
	// (usage_window, the stage's On set includes it), the chain falls to
	// the open stage, and the run serves. Parking it would defeat the
	// selection's own scenario (hard cap + closed forfait + healthy other
	// forfait). The reverse judgment (a capped CLAUDE_CODE ladder stage on
	// a primary that rides nothing meterable) is delivery-1 deferred: the
	// mid-run guard still covers claude_code stages in flight, and claw
	// stages were never meterable.
	// The MODE-only sandbox reading, on purpose (#2166): the materializing
	// screen refuses a codex stage on a run whose mode sandboxes it, from
	// the same two tiers, so the escape judgment agrees with the refusal it
	// mirrors. A driver-aware answer here would un-park a degraded-auto run
	// for a codex stage the screen still refuses, stranding it on the
	// capped wire — the very outcome the park prevents. The workflow is the
	// FLOORED one (#1959): the mode that counts is the one the run resolves
	// after a non-operator source's opt-out is neutralized.
	// The same reading tells the meter where a node resolves its credential
	// (runCredKeys.sandboxed): a sandboxed claw node in the container, over the
	// env the crossing hands it, a sandboxed pi node over the container's agent
	// dir. Mode-only there too: the driver-aware answer
	// (runtime.RunWillBeSandboxed) reads the checkout this admission has not
	// paid for yet, and on the runner an active mode starts the container — an
	// auto run degraded to the host is read as sandboxed, the residual.
	flooredWf, _ := runtime.ApplySandboxFloor(wf, msg.BotSourceTier, r.cfg.SandboxOverride)
	sandboxed := runtime.WorkflowSandboxActive(flooredWf, r.cfg.SandboxOverride, r.cfg.SandboxDefault)
	escapable := ladderEscapes(wf, msg, sandboxed)
	if len(routes) == 0 && len(sups) == 0 {
		if logger != nil {
			logger.Debug("runner: run %s targets no anthropic-wire route — usage cap not applied", msg.RunID)
		}
		return nil
	}
	// Each route is judged on the credential IT spends, not on the run's
	// default: a run holding a closed Claude forfait beside a key pinned for
	// its GLM routes can serve those routes, and a run whose default has room
	// may still route every node onto a walled key.
	capped := r.cappedRoutes(ctx, msg, routes, sups, sandboxed, pol, logger)
	for id := range escapable {
		delete(capped, id)
	}
	if len(capped) == 0 {
		return nil
	}
	d, blocked := parkDecision(wf, capped, open)
	if !blocked {
		if logger != nil {
			logger.Info("runner: run %s starts with %d capped route(s) it may avoid — the mid-run guard stops a claude_code call on a hard-capped one if the run takes it", msg.RunID, len(capped))
		}
		return nil
	}
	if logger != nil {
		logger.Warn("runner: run %s not started — %s", msg.RunID, d.Reason)
	}
	// The status flip is what makes the retry armable: ScheduleRunRetry
	// conditions on failed_resumable, and a run stopped before its first
	// node has never been marked anything. Without this the retry silently
	// fails to arm and the run is acked into nothing.
	if r.cfg.Store != nil {
		sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), usageCapStoreTimeout)
		defer scancel()
		sctx = store.WithIdentity(sctx, msg.TenantID, msg.OwnerID)
		if _, serr := r.cfg.Store.UpdateRunOutcome(sctx, msg.RunID, store.RunStatusFailedResumable,
			d.Reason,
			// Continuation deliberately unknown here: the arming
			// decision happens later (armUsageWindowRetry), and three
			// of its branches arm nothing — the document must not say
			// retry_armed before a retry actually exists. Promotion to
			// retry_armed lives with ScheduleRunRetry; demotion to
			// final with AbandonRunRetry.
			store.RunOutcomeMeta{Code: store.FailureUsageLimitBlocked},
			[]store.RunStatus{store.RunStatusRunning, store.RunStatusQueued}); serr != nil && logger != nil {
			logger.Warn("runner: usage-cap status flip for %s: %v", msg.RunID, serr)
		}
	}
	return &delegate.ErrRateLimited{
		Provider:    delegate.BackendClaudeCode,
		Detail:      fmt.Sprintf("%s (not started)", d.Reason),
		Kind:        delegate.RateLimitKindUsageWindow,
		ResetAt:     d.ResetsAt,
		SelfImposed: true,
	}
}

// cappedRoutes reads, once per meter key, the cap's verdict on the credential
// each primary route and each supervisor spends, and returns the capped ones by
// node id (supervisorRouteID for a supervisor). It fails OPEN per credential: a
// key the store cannot read is headroom, like the whole pre-flight is when the
// store is down. sandboxed is the run's sandbox (runCredKeys.sandboxed): where
// a node's backend resolves its credential decides what it spends.
func (r *Runner) cappedRoutes(ctx context.Context, msg *queue.RunMessage, routes []model.WireRoute, sups []supervisorRoute, sandboxed bool, pol usagecap.Policy, logger *iterlog.Logger) map[string]usagecap.Decision {
	keys := usageCapCredKeys(ctx, msg)
	keys.sandboxed = sandboxed
	rctx, cancel := context.WithTimeout(ctx, usageCapStoreTimeout)
	defer cancel()
	now := time.Now().UTC()
	byKey := map[string]usagecap.Decision{}
	capped := map[string]usagecap.Decision{}
	judge := func(id, key string, stoppable bool) {
		d, read := byKey[key]
		if !read {
			readings, err := r.cfg.UsageCaps.Latest(rctx, key)
			if err != nil {
				if logger != nil {
					logger.Warn("runner: usage-cap pre-flight read (%s): %v — proceeding on this credential", key, err)
				}
			} else {
				d = usagecap.Preflight(readings, pol, now, r.cfg.UsageCapTrust)
			}
			byKey[key] = d
		}
		if d.Blocked {
			if !stoppable {
				d.Stop = false
			}
			capped[id] = d
		}
	}
	for _, route := range routes {
		key, metered := keys.routeKey(ctx, route)
		if !metered {
			continue
		}
		// A hard cap stops a call in flight only through the readings the
		// mid-run guard observes, and only claude_code sessions report them —
		// pi only on a refusal, claw never. On any other backend, or one
		// resolved at dispatch, nothing would stop the capped call once the
		// run took the route: it parks the run as soon as it is reachable,
		// like a soft cap.
		judge(route.NodeID, key, route.Backend == delegate.BackendClaudeCode)
	}
	// A supervisor calls its model in process — claw, which reports no
	// readings — for the whole run: nothing stops it in flight either.
	for _, sup := range sups {
		key, metered := keys.supervisorKey(ctx, sup.spec, sup.hint)
		if !metered {
			continue
		}
		judge(sup.id, key, false)
	}
	return capped
}

// ladderEscapes names the nodes the adaptive-routing ladder lets off the
// anthropic wire: an eligible agent node (no routes of its own — the same
// eligibility the ladder's screen enforces) carrying a policy stage whose
// crossing lands on another wire. The node's capped primary then fails
// (usage_window — the stage's On carries it) and the chain serves the
// open stage: the run is not stranded on the capped wire, so it must not
// be parked for it.
func ladderEscapes(wf *ir.Workflow, msg *queue.RunMessage, sandboxed bool) map[string]bool {
	if wf == nil {
		return nil
	}
	var stages []queue.RunFallbackEntry
	for _, f := range msg.Fallback {
		if f.Policy {
			stages = append(stages, f)
		}
	}
	if len(stages) == 0 {
		return nil
	}
	escapes := map[string]bool{}
	// The ladder is RUN-LEVEL: every eligible node carries every stage, so
	// one SERVABLE off-wire stage escapes every eligible node. Servable is
	// hardened to what the materializing screen will not refuse outright:
	//   - the stage's On must carry usage_window — the capped primary's
	//     failure category; a stage that refuses it never catches the
	//     fall-through, and the park it deleted would have armed the
	//     durable retry;
	//   - a codex stage on a SANDBOXED run is refused by the screen
	//     (fallback_apply.go) — the runner's own sandbox tiers are known
	//     here, so the refusal is decided, not guessed;
	//   - an unmappable model (StageModel's no-mapping pairs) is dropped
	//     per node — checked against the node's own model.
	// RESIDUAL, said: the per-node predicates (ungated ask-rules, tools
	// inversion, session continuity) are not re-judged here — a stage
	// refused by one of them after its park was deleted converts the
	// durable retry into a bare failure. Delivery-1 boundary.
	ovs := modelOverridesFromMsg(msg.ModelOverrides)
	for _, st := range stages {
		if st.Backend == delegate.BackendClaudeCode {
			continue // the anthropic wire itself: no escape
		}
		if st.Backend == delegate.BackendClaw && (st.Provider == "anthropic_key" || st.Provider == "zai_key" || st.Provider == "moonshot_key") {
			continue // claw on an anthropic-wire key: still the capped wire
		}
		if !slices.Contains(st.On, "usage_window") {
			continue // the capped failure would not step to this stage
		}
		if st.Backend == "codex" && sandboxed {
			continue // the screen refuses it (fallback_apply.go:128)
		}
		for _, n := range wf.Nodes {
			nn, ok := n.(ir.LLMNode)
			if !ok || nn.NodeKind() != ir.NodeAgent || len(nn.GetFallbacks()) > 0 {
				continue
			}
			// The EFFECTIVE model — the override the launch stamped wins
			// over the declared field, exactly as the materializing
			// screen's own nodeModel callback reads it. A node with no
			// model of its own has NO ladder: the materializer drops its
			// stages (StageModel maps nothing modelless), so it cannot
			// escape off the capped wire — the park stands. A model that
			// maps nothing likewise drops the stage (named there) and is
			// no escape. (R27b623: the declared field alone disagreed
			// with the materializer whenever an override decided.)
			ov := ovs.ForNode(nn.NodeID(), nn.NodeKind())
			nodeModel := nn.GetLLMFields().Model
			if ov.Model != "" {
				nodeModel = ov.Model
			}
			if _, mappable, _ := llmroute.StageModel(st.Backend, st.Provider, nodeModel, llmroute.ResolveClasses(msg.FallbackModelClasses)); !mappable {
				continue
			}
			escapes[nn.NodeID()] = true
		}
	}
	return escapes
}

// supervisorRoute is one supervisor the run will spawn, the model spec its
// evaluator will resolve, and the provider hint it resolves that spec under
// (supervise.Spec.ProviderHint — the registry honours it as a node's own).
type supervisorRoute struct{ id, spec, hint string }

// preflightSupervisors lists the supervisors this run will spawn on this
// runner — none when the run-level override or ITERION_SUPERVISORS turns them
// off, the check the run itself makes — each with the model its evaluator
// resolves at its first evaluation (supervise.ResolveModel: the pin, the env
// default, then the provider the watched nodes run on and the detector, under
// the run's credentials). One that resolves no model fails every evaluation
// and spends nothing, and so does one no node of this run's topology is
// watched by (supervisorArms, reading `open`).
//
// The list is the one the spawn walks, pool filter included
// (runview.PoolSurvivingSpecs, the same call loop.go makes before
// supervise.StartDeclared): on a run stamped to a sovereign pool, a
// supervisor whose effective model is not the pool's gateway is REFUSED at
// spawn (D12), so metering it would park the run for spend no execution can
// make — the same rule supervisorArms applies to the watch set. A run with
// no pool stamp keeps every spec, which is every run but a pooled one.
//
// Both halves of that call get a logger, where a nil one discarded their two
// lines: a spec the filter refuses here WIDENS the walk — a run a capped
// supervisor would have parked starts instead — so the refusal that decided
// it is named, and so is a monitor the spec walk cannot parse, at admission
// rather than only at spawn.
//
// It is the ADMISSION logger, not the run's: usageCapPreflight runs before
// loop.go registers the run log writer, so these two lines land on the
// runner's own log and never in the `run.log` the studio and `iterion run
// log` read — where the spawn's identical lines do land. The caller forks it
// with the run id as a field for that reason; the filters' own text names the
// supervisor and not the run.
//
// vars is view.DecidedVars — the pre-flight's shared reading folded through
// the dispatch view, minus every name the reading leaves undecided: an
// engine-supplied consultation, and a late name (a default whose text
// consults the environment, a coercion failure kept raw — ir.LaunchVarsView's
// late set). Over the names that remain the reading is spawn's map
// (engine.ResolveVars — the resolved map SpecsFromWorkflow asks for): a pin
// over a literal default, a typed launch value or a stored input resolves
// here the way it resolves at spawn. A pin over an undecided name stays
// templated here, where model.ParseModelSpec refuses it in supervisorKey:
// the route meters nothing, and this walk answers "not metered" where the
// node walks answer "reachable" on an unresolvable reading. Matching spawn
// on a late name would park, or spare, the run on the reading process's own
// environment — the answer the view's field reading refuses to stand for
// the run (ADR-129 §4, rule 1).
func preflightSupervisors(ctx context.Context, wf *ir.Workflow, msg *queue.RunMessage, vars map[string]any, open ir.EdgeFilter, logger *iterlog.Logger) []supervisorRoute {
	if wf == nil || len(wf.Supervisors) == 0 {
		return nil
	}
	if enabled, _ := supervise.DeclaredEnabled(msg.Supervisors); !enabled {
		return nil
	}
	var out []supervisorRoute
	ambientRefusedWalk := false
	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		ambientRefusedWalk = creds.AmbientRefused
	}
	for i, spec := range runview.AmbientSurvivingSpecs(runview.PoolSurvivingSpecs(supervise.SpecsFromWorkflow(wf, vars, logger), msg.ContentBoundary, msg.RunnerPool, logger), ambientRefusedWalk, logger) {
		if !supervisorArms(wf, open, spec.Watches) {
			continue
		}
		// A gateway-watched supervisor with no pin and no env default
		// resolves the VENDOR family here while its evaluator refuses every
		// evaluation (supervise.ErrGatewayWatchNeedsPin): metering it under a
		// hard cap could park the run for spend nothing will make. The two
		// sites must agree that this supervisor spends nothing.
		if spec.GatewayWatched &&
			strings.TrimSpace(ir.ExpandEnvWithDefault(spec.Model)) == "" &&
			ir.LookupEnv("ITERION_DEFAULT_SUPERVISOR_MODEL") == "" {
			continue
		}
		resolved, err := supervise.ResolveModel(ctx, spec.Model, spec.ProviderHint)
		if err != nil {
			continue
		}
		out = append(out, supervisorRoute{id: supervisorRouteID(spec.Name, i), spec: resolved, hint: spec.ProviderHint})
	}
	return out
}

// supervisorArms reports whether this run can arm the supervisor at all —
// whether some node it WATCHES is on the run's topology, read through the
// same launch-vars filter the park decision reads.
//
// A supervisor is otherwise counted as a route on every execution, and
// nothing stops it in flight, so a capped one parks the run on its own. That
// is true only while it can evaluate: a coordinator evaluates nothing unless
// one of its watched nodes is ACTIVE (supervise.Coordinator.armed), so a
// supervisor whose every watched node the launch excluded — a bi-model bot's
// per-family pacer, the family this run does not take — never spawns a call
// and spends nothing. Metering it would park the run for spend no execution
// can make, which is the rule #1828's prune established for nodes.
//
// An EMPTY watch set is the whole run (armed from the first node), and so is
// the answer here. A watched id the graph does not hold never matches an
// active node either, so it arms nothing — the same verdict for the same
// reason. With no launch reading `open` is nil and this is the whole-graph
// answer: every node the entry can reach stands.
func supervisorArms(wf *ir.Workflow, open ir.EdgeFilter, watches []string) bool {
	if wf == nil || len(watches) == 0 {
		return true
	}
	for _, id := range watches {
		if wf.CanReachVia(open, func(n ir.Node) bool { return n != nil && n.NodeID() == id }) {
			return true
		}
	}
	return false
}

// supervisorRouteIDPrefix marks a capped route that is a supervisor, not a
// node: it is on every execution of the run. A node id is a DSL identifier,
// which never holds the colon.
const supervisorRouteIDPrefix = "supervisor:"

func supervisorRouteID(name string, i int) string {
	if name != "" {
		return supervisorRouteIDPrefix + name
	}
	return fmt.Sprintf("%s#%d", supervisorRouteIDPrefix, i)
}

// supervisorKey is the meter key a supervisor's resolved model spec spends,
// and whether the cap meters it at all. It spends in process, in
// Registry.ResolveWithContext's order under the hint its evaluator resolves
// with (the watched nodes' provider family, supervise.Spec.ProviderHint):
// `anthropic/…` (no GLM id) the Anthropic key held for the route, else the
// Claude forfait while claw takes it — not under
// ITERION_FORBID_SUBSCRIPTION_OAUTH, not behind a non-Anthropic destination
// (model.RunCredentialBaseURL: the env's ANTHROPIC_BASE_URL, the direct wire
// under the `provider: "anthropic"` hint) — else the pod env, read off claw's
// own walk (clawUnslottedKey): the account its client carries, the
// no-credential meter where it carries none of the env's, nothing where claw
// refuses the walk. A GLM id there the run's held z.ai key, else the run's
// Anthropic key — the keyed factory past the z.ai branch, at the env's
// destination, not metered where that is the real Anthropic wire — else that
// same walk (clawUnslottedKey holds both, in that order): the pod's
// ZAI_API_KEY synthesis, a destination that serves no GLM id and a request
// with no credential spend no wall this cap keeps; the account the request
// carries otherwise. A claw supervisor reports no readings of its own, so the
// account's refusals the other lanes record are the only evidence it has.
// `zai/…` and `moonshot/…` that provider's key, a gateway spec the run's
// gateway pair; with none held the registry reads each provider's OWN pod env
// (MOONSHOT_API_KEY, the OPENAI_COMPATIBLE pair) or refuses — never the pod's
// anthropic ambient env — so they are not metered either. A spec the registry
// cannot parse spends nothing; any other provider is off the wire the cap
// meters.
func (k runCredKeys) supervisorKey(ctx context.Context, spec, hint string) (key string, metered bool) {
	// Read as the registry reads it: untrimmed, case-sensitive.
	provider, _, err := model.ParseModelSpec(spec)
	if err != nil {
		return "", false
	}
	creds, _ := secrets.CredentialsFromContext(ctx)
	var slot string
	switch secrets.Provider(provider) {
	case secrets.ProviderAnthropic:
		if model.GLMOnAnthropicWire(spec) {
			if slot = heldForRoute(creds, string(secrets.ProviderZAI)); slot == "" {
				// No z.ai key of the run: the run's Anthropic key, else the
				// pod env, in process.
				return k.clawUnslottedKey(creds, spec, hint, false)
			}
			return k.keyForSlot(slot), true
		}
		slot = heldForRoute(creds, string(secrets.ProviderAnthropic))
		if slot == "" && creds.OAuthDir(string(secrets.OAuthKindClaudeCode)) != "" &&
			!secrets.ForbidSubscriptionOAuth() && secrets.AnthropicForfaitWireOK(model.RunCredentialBaseURL(spec, hint)) {
			slot = string(secrets.OAuthKindClaudeCode)
		}
		if slot == "" {
			// Nothing the run holds funds this spec: claw resolves it from
			// the pod env, in process.
			return k.clawUnslottedKey(creds, spec, hint, false)
		}
	case secrets.ProviderZAI, secrets.ProviderMoonshot:
		slot = heldForRoute(creds, provider)
	case secrets.ProviderOpenAICompatible:
		// The run's gateway slot: a held pair meters on the credential the
		// registry serves (the uncached arm), the same rule the vendor
		// slots follow.
		slot = heldForRoute(creds, provider)
	default:
		return "", false
	}
	if slot == "" {
		// Nothing the run holds funds this spec either, and none of these
		// providers walks into the pod's anthropic ambient env: the registry
		// reads each from its own pod environment (ZAI_API_KEY — no registry
		// factory exists for zai at all, so the spec is refused —
		// MOONSHOT_API_KEY, the deployment's OPENAI_COMPATIBLE pair) or
		// refuses. Keying the spec on the ambient account's meter would park
		// the run on a wall another vendor's account raised. Not metered.
		return "", false
	}
	return k.keyForSlot(slot), true
}

// launchReading resolves the launch values the pre-flight's walks read —
// the wire view (whose DecidedVars the supervisor walk resolves the pins
// against), the edge filter — once, together: the message's own
// launch values where it carries them; a resume PUBLICATION's stored inputs
// otherwise (the attempt it continues resumes from the checkpoint those
// inputs built); the declared defaults where a fresh launch overrode nothing
// (nil folds them, the run's own resolution). ok is false only when a
// resume's inputs cannot be read: every consumer then keeps the undecided
// answer — a var-less view, the whole-graph walk, pins left templated —
// never defaults that may not be this run's.
//
// What the message carries is not what the message IS: `Resume` is stamped by
// the runner itself on a REDELIVERED launch (resolveDeliveryPreconditions,
// queue.Admit's AsResume), which still holds this run's launch vars — the
// first park's own status flip puts the run in exactly that state. So the
// vars decide, not the shape: a redelivery reads its own, a publication reads
// the run document's Inputs. Reading the document is what makes the retry
// honour the instant the park promised: a woken resume must walk the same
// topology the launch did, or it parks again past it.
func (r *Runner) launchReading(ctx context.Context, msg *queue.RunMessage, logger *iterlog.Logger) (map[string]any, bool) {
	if msg == nil {
		return nil, false
	}
	if len(msg.Vars) > 0 {
		return msg.Vars, true
	}
	if msg.Resume == nil {
		// A fresh launch overrode nothing: the declared defaults are the
		// values the run itself resolves.
		return nil, true
	}
	return r.storedLaunchVars(ctx, msg, logger)
}

// launchVarEdges is the edge filter the park walk reads THIS RUN's topology
// through: a branch the run's own launch vars exclude is not a branch it may
// take, so a capped route standing on one parks nothing
// (ir.Workflow.LaunchVarEdgeFilter decides which transitions qualify).
// Both Via walks derive the same node-end annotation from this filter; an
// excluded exit is never restored to disguise a node's own death as progress.
// launch is the pre-flight's shared reading (launchReading); !have keeps the
// whole-graph answer — folding declared defaults would speak for a launch
// that may have overridden them.
func launchVarEdges(wf *ir.Workflow, launch map[string]any, have bool) ir.EdgeFilter {
	if wf == nil || !have {
		return nil
	}
	return wf.LaunchVarEdgeFilter(launch)
}

// storedLaunchVars reads the launch inputs off the run document — the vars a
// resume publication does not carry. False when there is no document to read:
// the caller keeps the whole-graph answer rather than folding defaults that
// may not be this run's, and says why.
func (r *Runner) storedLaunchVars(ctx context.Context, msg *queue.RunMessage, logger *iterlog.Logger) (map[string]any, bool) {
	if r.cfg.Store == nil {
		return nil, false
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageCapStoreTimeout)
	defer cancel()
	rctx = store.WithIdentity(rctx, msg.TenantID, msg.OwnerID)
	run, err := r.cfg.Store.LoadRun(rctx, msg.RunID)
	if err != nil || run == nil {
		if logger != nil {
			logger.Warn("runner: run %s: launch inputs unreadable for the usage-cap walk (%v) — judging the whole graph, which may park a run whose launch vars exclude the capped route", msg.RunID, err)
		}
		return nil, false
	}
	return run.Inputs, true
}

// parkDecision answers whether a run cannot start, and when it can.
//
// A HARD-capped route parks the run only when it cannot be avoided — every
// execution from the entry to a terminal crosses a capped route
// (ir.Workflow.AlwaysReachesVia, fan-out branches all taken): a hard cap stops
// a call in flight, so a run with a path around it may start and be stopped
// there if it goes that way. A SOFT cap stops nothing in flight — it only
// refuses NEW work — so a soft-capped route the run may reach at all
// (ir.Workflow.CanReachVia) parks it: letting the run through to find out
// would spend it uninterrupted. So does a hard cap on a route no in-flight
// guard can stop (cappedRoutes clears its Stop). A capped supervisor parks the
// run whatever its paths: it runs on every execution.
//
// Both walks read the run's own topology: open drops the transitions its
// launch vars already excluded (launchVarEdges), so neither a route the run
// cannot reach parks it, nor a branch it cannot take frees it.
//
// The retry is armed for the earliest reopening after which neither holds —
// coming back later waits for nothing, coming back earlier parks again. A
// capped route that did not say when it reopens stays capped throughout; when
// such routes keep the run parked, it parks on one of them with no known
// reopening.
//
// Deterministic whatever order the routes were read in: the candidates are
// sorted by instant, then by node id.
func parkDecision(wf *ir.Workflow, capped map[string]usagecap.Decision, open ir.EdgeFilter) (usagecap.Decision, bool) {
	ids := make([]string, 0, len(capped))
	for id := range capped {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	stillCapped := func(d usagecap.Decision, t time.Time) bool {
		return t.IsZero() || d.ResetsAt.IsZero() || d.ResetsAt.After(t)
	}
	cappedAt := func(n ir.Node, t time.Time, softOnly bool) bool {
		d, ok := capped[n.NodeID()]
		if !ok || (softOnly && d.Stop) {
			return false
		}
		return stillCapped(d, t)
	}
	parkedAt := func(t time.Time) bool {
		// A supervisor is on every execution, and nothing stops it in flight.
		for _, id := range ids {
			if strings.HasPrefix(id, supervisorRouteIDPrefix) && stillCapped(capped[id], t) {
				return true
			}
		}
		if wf.CanReachVia(open, func(n ir.Node) bool { return cappedAt(n, t, true) }) {
			return true
		}
		return wf.AlwaysReachesVia(open, func(n ir.Node) bool { return cappedAt(n, t, false) })
	}
	if !parkedAt(time.Time{}) {
		return usagecap.Decision{}, false
	}
	var resets []time.Time
	for _, id := range ids {
		if at := capped[id].ResetsAt; !at.IsZero() && !slices.ContainsFunc(resets, at.Equal) {
			resets = append(resets, at)
		}
	}
	slices.SortFunc(resets, func(a, b time.Time) int { return a.Compare(b) })
	// What the narrowed walk accepted, not the candidate set: a capped route
	// on a branch the launch excluded is on no path of this run, and the
	// operator reads this number in the run document's failure reason.
	// A supervisor is on every execution and is no graph node, so it counts
	// without being walked to.
	onThePaths := 0
	for _, id := range ids {
		if strings.HasPrefix(id, supervisorRouteIDPrefix) ||
			wf.CanReachVia(open, func(n ir.Node) bool { return n != nil && n.NodeID() == id }) {
			onThePaths++
		}
	}
	pick := func(match func(usagecap.Decision) bool) usagecap.Decision {
		for _, id := range ids {
			if d := capped[id]; match(d) {
				if onThePaths > 1 {
					d.Reason += fmt.Sprintf(" — %d capped routes on the run's paths", onThePaths)
				}
				return d
			}
		}
		return usagecap.Decision{}
	}
	for _, t := range resets {
		if !parkedAt(t) {
			return pick(func(d usagecap.Decision) bool { return d.ResetsAt.Equal(t) }), true
		}
	}
	// No known reopening frees the run: a capped route with no known
	// reopening keeps it parked, or the graph cannot be walked (then the last
	// reopening is when every capped route has room again).
	for _, id := range ids {
		if capped[id].ResetsAt.IsZero() {
			return pick(func(d usagecap.Decision) bool { return d.ResetsAt.IsZero() }), true
		}
	}
	last := resets[len(resets)-1]
	return pick(func(d usagecap.Decision) bool { return d.ResetsAt.Equal(last) }), true
}

// admitAttempt is the last gate before an attempt can spend anything, and
// the moment it takes hold of what it will spend. The pre-flight decides
// whether the run may start at all; only once it has, the attempt stamps
// its credentials as held, so a multi-hour attempt does not read as an
// idle key for its whole duration (#659 pt 2).
//
// The order is the point, both ways: a run parked on a ceiling never held
// anything (stamping it would date a key that served nothing), and a run
// that starts must not wait until it ends to say which key it is spending.
func (r *Runner) admitAttempt(ctx context.Context, wf *ir.Workflow, msg *queue.RunMessage) error {
	if err := r.usageCapPreflight(ctx, wf, msg, r.cfg.Logger); err != nil {
		return err
	}
	r.markCredFingerprintsUsed(ctx, msg, time.Now().UTC())
	return nil
}
