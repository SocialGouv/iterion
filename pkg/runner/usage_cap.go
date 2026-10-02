package runner

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
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
// to the bundle-default precedence, secrets.AnthropicWireSlotOrder —
// anthropicCredEnvForCLI's contract, read from the list it is written
// against. A rotated token therefore opens a fresh meter instead of
// inheriting the readings of the account it replaced.
//
// "anthropic-env" is the POD's inherited env: the session carried no bundle
// credential at all — an `anthropic`-pinned node on a run whose wire is held
// by a z.ai key reaches it, and on a pod with no ambient auth its "Not
// logged in" is a refusal. It is keyed on the scope with no credential,
// never on the bundle default: charged to the head of the precedence, that
// refusal would bench a healthy z.ai key the session never touched.
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
// all, and there the bundle default is the only answer available.
func (k runCredKeys) forSource(source string) string {
	return k.keyForSlot(k.slotForSource(source))
}

// slotForSource names the slot a reading's source label is charged to — ""
// when no bundle credential ran — by the rules forSource documents.
func (k runCredKeys) slotForSource(source string) string {
	switch {
	case strings.HasPrefix(source, "facade:"):
		if s := delegate.AnthropicWireFacadeSlot(source); s != "" {
			return s
		}
	case source == "anthropic-direct":
		if k.anthropicFP != "" {
			return string(secrets.ProviderAnthropic)
		}
		return ""
	case source == "anthropic-oauth":
		if k.oauthFP != "" {
			return string(secrets.OAuthKindClaudeCode)
		}
		return ""
	case source == "anthropic-env":
		return ""
	}
	_, slot := k.firstHeld()
	return slot
}

// keyForSlot is the meter key the readings of one credential slot land on:
// the slot's own ledger scope — a shared tier's pinned key is metered on its
// owner's ledger even when the run itself is tenant-scoped — and its
// fingerprint. "" is the run's scope with no credential, where a session on
// the pod's ambient env records.
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
// spends nothing and is not metered. pi's hint names the provider it spends,
// over the model's prefix; kimi and grok spend their own config and are not
// metered; other backends route on the spec, as the spend ledger books them
// (credentialSlotForRoute) — opencode, which books on nobody, on the pod's
// ambient meter. A slot off the anthropic wire is not what the cap meters. A
// route the walk cannot read keeps the run's default credential — what the
// pre-flight read before it read routes.
func (k runCredKeys) routeKey(ctx context.Context, route model.WireRoute) (key string, metered bool) {
	if !route.Readable {
		return k.forSource(""), true
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
	return k.keyForSlot(slot), true
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

// usageCapKey is the run's DEFAULT credential key — the one a session with no
// source label is charged to, and what the pre-flight reads for a route it
// cannot read (runCredKeys.routeKey).
func usageCapKey(ctx context.Context, msg *queue.RunMessage) string {
	return usageCapCredKeys(ctx, msg).forSource("")
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
func (r *Runner) usageCapPreflight(ctx context.Context, wf *ir.Workflow, msg *queue.RunMessage, logger *iterlog.Logger) error {
	if r.cfg.UsageCapSource == nil || r.cfg.UsageCaps == nil {
		return nil
	}
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
	// justify refusing the run before it starts. Every uncertainty
	// answers "reachable". A supervisor the run will spawn is a route too —
	// one every execution takes (supervisorRouteID).
	routes := model.AnthropicWireRoutes(wf, modelOverridesFromMsg(msg.ModelOverrides))
	sups := preflightSupervisors(ctx, wf, msg)
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
	capped := r.cappedRoutes(ctx, msg, routes, sups, pol, logger)
	if len(capped) == 0 {
		return nil
	}
	d, blocked := parkDecision(wf, capped)
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
// store is down.
func (r *Runner) cappedRoutes(ctx context.Context, msg *queue.RunMessage, routes []model.WireRoute, sups []supervisorRoute, pol usagecap.Policy, logger *iterlog.Logger) map[string]usagecap.Decision {
	keys := usageCapCredKeys(ctx, msg)
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
		key, metered := keys.supervisorKey(ctx, sup.spec)
		if !metered {
			continue
		}
		judge(sup.id, key, false)
	}
	return capped
}

// supervisorRoute is one supervisor the run will spawn, and the model spec its
// evaluator will resolve.
type supervisorRoute struct{ id, spec string }

// preflightSupervisors lists the supervisors this run will spawn on this
// runner — none when the run-level override or ITERION_SUPERVISORS turns them
// off, the check the run itself makes — each with the model its evaluator
// resolves at its first evaluation (supervise.ResolveModel: the pin, the env
// default, then the provider the watched nodes run on and the detector, under
// the run's credentials). One that resolves no model fails every evaluation
// and spends nothing.
func preflightSupervisors(ctx context.Context, wf *ir.Workflow, msg *queue.RunMessage) []supervisorRoute {
	if wf == nil || len(wf.Supervisors) == 0 {
		return nil
	}
	if enabled, _ := supervise.DeclaredEnabled(msg.Supervisors); !enabled {
		return nil
	}
	var out []supervisorRoute
	for i, spec := range supervise.SpecsFromWorkflow(wf, nil) {
		resolved, err := supervise.ResolveModel(ctx, spec.Model, spec.ProviderHint)
		if err != nil {
			continue
		}
		out = append(out, supervisorRoute{id: supervisorRouteID(spec.Name, i), spec: resolved})
	}
	return out
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
// Registry.ResolveWithContext's order: `anthropic/…` the Anthropic key held for
// the route, else the Claude forfait while the claw factory takes it (not under
// ITERION_FORBID_SUBSCRIPTION_OAUTH, not behind a non-Anthropic
// ANTHROPIC_BASE_URL — anthropicFromCtxForfait), else the pod's ambient env —
// a GLM id there the z.ai key first; `zai/…` and `moonshot/…` that provider's
// key, else the ambient env. A spec the registry cannot parse spends nothing;
// any other provider is off the wire the cap meters.
func (k runCredKeys) supervisorKey(ctx context.Context, spec string) (key string, metered bool) {
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
			slot = heldForRoute(creds, string(secrets.ProviderZAI))
		}
		if slot == "" {
			slot = heldForRoute(creds, string(secrets.ProviderAnthropic))
		}
		if slot == "" && creds.OAuthDir(string(secrets.OAuthKindClaudeCode)) != "" &&
			!secrets.ForbidSubscriptionOAuth() && secrets.AnthropicForfaitWireOK(os.Getenv("ANTHROPIC_BASE_URL")) {
			slot = string(secrets.OAuthKindClaudeCode)
		}
	case secrets.ProviderZAI, secrets.ProviderMoonshot:
		slot = heldForRoute(creds, provider)
	default:
		return "", false
	}
	return k.keyForSlot(slot), true
}

// parkDecision answers whether a run cannot start, and when it can.
//
// A HARD-capped route parks the run only when it cannot be avoided — every
// execution from the entry to a terminal crosses a capped route
// (ir.Workflow.AlwaysReaches, fan-out branches all taken): a hard cap stops a
// call in flight, so a run with a path around it may start and be stopped
// there if it goes that way. A SOFT cap stops nothing in flight — it only
// refuses NEW work — so a soft-capped route the run may reach at all
// (ir.Workflow.CanReach) parks it: letting the run through to find out would
// spend it uninterrupted. So does a hard cap on a route no in-flight guard
// can stop (cappedRoutes clears its Stop). A capped supervisor parks the run
// whatever its paths: it runs on every execution.
//
// The retry is armed for the earliest reopening after which neither holds —
// coming back later waits for nothing, coming back earlier parks again. A
// capped route that did not say when it reopens stays capped throughout; when
// such routes keep the run parked, it parks on one of them with no known
// reopening.
//
// Deterministic whatever order the routes were read in: the candidates are
// sorted by instant, then by node id.
func parkDecision(wf *ir.Workflow, capped map[string]usagecap.Decision) (usagecap.Decision, bool) {
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
		if wf.CanReach(func(n ir.Node) bool { return cappedAt(n, t, true) }) {
			return true
		}
		return wf.AlwaysReaches(func(n ir.Node) bool { return cappedAt(n, t, false) })
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
	pick := func(match func(usagecap.Decision) bool) usagecap.Decision {
		for _, id := range ids {
			if d := capped[id]; match(d) {
				if len(capped) > 1 {
					d.Reason += fmt.Sprintf(" — %d capped routes on the run's paths", len(capped))
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
