package model

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// OverrideEntry is the neutral shape every model-override folder passes
// in. `runview.ModelOverrideEntry`, `store.RunModelOverride` and
// `queue.ModelOverride` are field-identical — a tiny adapter per site
// beats importing three types here.
type OverrideEntry struct {
	Selector string
	Backend  string
	Model    string
	Provider string
	Effort   string
}

// OverridesFrom folds neutral entries into the executor's per-node
// overrides. It is the ONE fold: runview (launch), the cloud publisher
// (launch + resume) and the runner (wire) all adapt onto it, so a cloud
// run resolves per-node models exactly like a local launch with the same
// flags — and a judge-kind selector cannot be honoured on one surface and
// dropped on another (#668).
func OverridesFrom(entries []OverrideEntry) ModelOverrides {
	var o ModelOverrides
	for _, e := range entries {
		if e.Backend != "" {
			o.SetBackend(e.Selector, e.Backend)
		}
		if e.Model != "" {
			o.SetModel(e.Selector, e.Model)
		}
		if e.Provider != "" {
			o.SetProvider(e.Selector, e.Provider)
		}
		if e.Effort != "" {
			o.SetEffort(e.Selector, e.Effort)
		}
	}
	return o
}

// FallbackEntry is the neutral run-level fallback-chain shape the
// derivation consumes. `runview.FallbackEntry`, `store.RunFallbackEntry`
// and `queue.RunFallbackEntry` are field-identical on (Backend, Model,
// Provider); a per-node `fallbacks:` block is read directly through
// `ir.LLMNode.GetFallbacks()` and does not use this type.
type FallbackEntry struct {
	Backend  string
	Model    string
	Provider string
}

// ProviderResolution is what EffectiveProviders learned about a run.
type ProviderResolution struct {
	// Providers is the union of KNOWN provider names some route of the
	// run may spend: every LLM node under its overrides, every node
	// `fallbacks:` route, every supervisor, the run-level chain. Sorted, deduplicated,
	// lower-cased. Names the caller does not know never enter it.
	Providers []string
	// NarrowSafe is false when some route the run may take could not be
	// resolved to a known provider: an LLM node with no pin, an explicit
	// "auto", a ${VAR} empty at resolution time, a hint the caller does
	// not know, a model-calling node that carries no LLMFields at all
	// (a subbot — a human node's LLM half and a Verified Action's
	// recovery rungs are READ, through humanLLMModelOf and
	// recoveryRungsOf), or a fallback route that pins nothing. A caller
	// NARROWING a request on Providers
	// must fail OPEN when it is false — treat the run as able to spend
	// every provider — because a node that resolved to nothing takes
	// whatever credential the process holds (claw substitutes the first
	// available provider; claude_code walks its bundle precedence).
	NarrowSafe bool
	// EnvDeferred reports a route whose own text defers to an environment or
	// to the run's vars (envDeferred): what it spends is decided where the
	// run executes, even when this process's expansion reads narrow-safe — a
	// `${DIAL:-default}` whose default names a vendor may expand to the
	// gateway on the runner. Such a route is never env-funded nor
	// key-attributed, and a reader that must not drop a credential the
	// runner's expansion may spend reads this beside NarrowSafe.
	EnvDeferred bool
	// Unknown lists the hints that named something and matched no known
	// provider — a typo in `provider:`, a model prefix the pool never
	// lends — so the caller can say WHICH pin made it widen. Sorted.
	Unknown []string
	// EnvFunded lists the gateway-served prefixes the walk read
	// (openai_compatible/…) that no sealed key funds — spend from the
	// deployment's environment, not from a credential. A gateway route the
	// view attributes to a key (RouteView.GatewayKeyFunded) is in Providers
	// instead. It does NOT force NarrowSafe false: the route acquires no
	// credential, so narrowing to the named providers is exact
	// (OnlyEnvFunded is the predicate the publisher spends). Sorted.
	EnvFunded []string
	// ForfaitFirst lists the providers of Providers whose EVERY route runs on
	// a backend that spends the provider's forfait before a key pinned for
	// the route (delegate.RegisterForfaitFirst). A positive list: a route on
	// an unknown backend, a direct generation, a run-level fallback with no
	// backend of its own keeps its provider out, and the zero value claims
	// nothing. Sorted.
	ForfaitFirst []string
	// Pins maps a provider in Providers to the sites that named it — the
	// node id whose own route resolves to it, `node#name` for an authored
	// `fallbacks:` route (its index when the author named none),
	// `node#interaction` for an interaction generation, `node#self_repair`
	// and `node#agent_recovery` for a Verified Action's recovery rungs,
	// `run-fallback#<stage>` for the run-level chain, `supervisor:<name>`
	// for a supervisor. A refusal built on this walk can say which node
	// pinned what; sorted, deduplicated.
	Pins map[string][]string
	// Sites maps each site to the KNOWN providers its routes name, in the
	// order the walk read them — the chain order the executor resolves, so
	// a reader that must pick "the credential this site spends" walks the
	// list the same way. Sorted pins answer "who named this provider";
	// Sites answers "what does this site name", which is the direction the
	// credential plan (#2288) reads. First occurrence wins, so a chain that
	// names a provider twice holds it once.
	Sites map[string][]string
	// Forbidden maps a provider OUTSIDE the view's allowlist
	// (RouteView.AllowedProviders, ADR-129 §5) to the sites that named it —
	// the same site keys as Pins. A forbidden provider is excluded from
	// Providers and Pins: no credential is sealed for it and no unfunded
	// refusal names it. Populated only when an allowlist is in force;
	// sorted, deduplicated.
	Forbidden map[string][]string
	// ForbiddenPrimary holds the subset of Forbidden named by a PRIMARY
	// route — the node's own chain, an interaction generation, a supervisor
	// — that no allowed provider served. This is the launch refusal
	// (ErrProviderOutsideAllowlist): there is no way to serve that node
	// inside the allowlist, and "refused, never re-routed" is the contract.
	// A rescue route outside the list lands in Forbidden alone and is
	// pruned, never refused. Populated only when an allowlist is in force;
	// sorted, deduplicated.
	ForbiddenPrimary map[string][]string
	// Unreadable lists the routes the walk could not read while an
	// allowlist was in force — a `{{vars.*}}` or empty-`${…}` field, a
	// prefix-less model, a hint outside the vocabulary, a model-calling
	// node that exposes no LLM fields. Under an allowlist, readability is
	// compulsory: every entry is a launch refusal
	// (ErrRouteUnreadableUnderAllowlist), because a lock that cannot be
	// checked must not pass. Empty without an allowlist (the walk's
	// posture there is the pre-allowlist widen). Sorted by site, field.
	Unreadable []UnreadableRoute
	// envFundedOnly reports whether every LLM route the walk read resolved
	// to the openai_compatible gateway — a route whose credential the
	// RUNNER's environment funds, never the bundle. A route that resolved
	// to any bundle-funded provider (a gateway route the view attributes to
	// a sealed key included), a route that could not resolve (an "auto", an
	// empty ${VAR}, a name the vocabulary does not know, a model-answering
	// node without fields), and a run with no LLM route at all, all read
	// false.
	envFundedOnly bool
	// AnthropicWireDefaultReads lists the anthropic-wire key slots some route
	// may spend as the run's DEFAULT credential — through the delegates'
	// default precedence — rather than as a key its hint or spec names. A
	// claude_code route reads every slot unless each element of its chain is
	// a hint it honours (anthropic, zai, moonshot) and nothing in it or its
	// model is read late — from the environment, which the runner expands
	// with its own, or from the run's vars at dispatch; a hint-less GLM id
	// is z.ai's only while a z.ai key is reachable, so it reads every slot
	// but zai. A claw
	// route on the anthropic wire reads the slots its anthropic provider
	// spends: the Anthropic key, and — sandboxed — the z.ai key, never a
	// Moonshot one. A route whose backend resolves at dispatch reads what
	// either would. A route the walk cannot resolve (NarrowSafe false) may
	// read any. Sorted.
	AnthropicWireDefaultReads []string
	// GatewaySites lists the sites (Pins' site keys) whose route this walk
	// read as the openai_compatible gateway — a literal `openai_compatible/…`
	// model, a gateway hint over one, or an env-templated model that expanded
	// to one here: the routes EnvFunded names the prefix of. Such a route
	// acquires no credential from the bundle; the runner's environment funds
	// it. A gateway route the allowlist pruned is not one, nor is one a
	// sealed key funds (RouteView.GatewayKeyFunded): that route names the
	// openai_compatible provider in Providers instead. Sorted.
	GatewaySites []string
	// DeferredSites lists the sites whose route is decided by the process
	// that DISPATCHES it rather than by this reading: a deciding routing
	// field — the provider override (or, without one, the node's provider
	// chain), the effective model, the backend the route runs on — that
	// reads the environment (`${…}`, which the runner expands with its OWN
	// environment and settings overlay; a runner pool's env block pins its
	// names ahead of the platform record) or a run var this view left late
	// (`{{vars.…}}`). Whatever this walk recorded for such a site is this
	// process's expansion, not the runner's word. Sorted.
	DeferredSites []string
	// UnattributedSites lists the sites at which the walk widened — every
	// route that flips NarrowSafe false: a prefix-less model, an `auto` or
	// empty step, a hint outside the vocabulary, a `${…}` that expands to
	// nothing here, a `{{vars.…}}` the view left late, a model-calling node
	// that exposes no LLM fields (a subbot, whose child bot this walk never
	// reads), a supervisor with no model. Recorded with or without an
	// allowlist: NarrowSafe says THAT some route may spend anything, this
	// says WHICH. Sorted.
	UnattributedSites []string
	// WireDefaultSites maps each site whose route may read an anthropic-wire
	// slot as the run's DEFAULT credential to the slots it may read — the
	// per-site reading AnthropicWireDefaultReads is the union of, committed
	// under the same rules (a pruned route reads nothing, a slot outside
	// the allowlist is read by no route). A surviving route whose every slot
	// the allowlist excludes maps to an EMPTY list, never to no entry: it
	// still walks the wire's default on the runner, finds no slot it may
	// spend, and is refused there — a funding reader must see it. Each list
	// sorted.
	WireDefaultSites map[string][]string
}

// OnlyEnvFunded reports whether the run's every LLM route is an
// openai_compatible route the runner's environment funds — no sealed key
// does (RouteView.GatewayKeyFunded) — so the resolution acquires no LLM
// credential for it. See ProviderResolution.envFundedOnly.
func (r ProviderResolution) OnlyEnvFunded() bool { return r.envFundedOnly }

// EffectiveProviders is the walk every "which providers does this run
// actually target?" question goes through — the pool's wants derivation
// and any future site that needs the answer. It reads the DSL under
// launch-time overrides, node `fallbacks:` blocks, the supervisors and the
// run-level fallback chain, resolved like the executor's own resolveProviderChain:
// `ir.ExpandEnvWithDefault` → split on `,` → `ir.SplitProviderStep` →
// `auto` means unresolved. A provider override collapses the chain; a
// `provider/model` prefix on the effective model counts as a pin.
//
// The routes are read through the view (RouteView): a `{{vars.*}}` span
// resolves against the run's vars the way dispatch resolves it, a late
// name stays as written (undecided), and a `fallbacks:` route whose
// `when:` gate the view answers FALSE leaves the walk — it is not a path
// the run may take. A gate the view cannot answer keeps the route
// (FallbackWhenActive's posture): the walk never drops a provider the run
// might spend.
//
// `known` is the caller's provider vocabulary. A nil workflow or an empty
// `known` yields the zero value (NarrowSafe false), which every caller
// reads as "fail open".
func EffectiveProviders(v RouteView, known map[string]bool) ProviderResolution {
	wf := v.Workflow
	if wf == nil || len(known) == 0 {
		return ProviderResolution{}
	}
	acc := &providerAccumulator{known: known, providers: map[string]bool{}, unknown: map[string]bool{}, forfaitFirst: map[string]bool{}, pins: map[string][]string{}, narrowSafe: true, vars: v.Vars, late: v.LateVars, allowed: AllowedProviderSet(v.AllowedProviders), gatewayKeyFunded: v.GatewayKeyFunded}
	gateVars := v.gateVars()
	seenAnyLLM := false
	// A run-level fallback stage naming no backend runs on each node's own,
	// so it is late wherever some node's backend is.
	anyNodeBackendLate := false
	for _, n := range wf.Nodes {
		fields, ok := llmFieldsOf(n)
		if !ok {
			if mdl, human := humanLLMModelOf(n); human {
				// The human node's LLM half (#2691): a direct generation on
				// node.Model, read as the interaction arm below reads an
				// agent's — the spec's own prefix routes, no hint, no
				// backend, and no override cell (executeHumanLLMWithSpend
				// never consults ForNode).
				seenAnyLLM = true
				acc.backend, acc.backendLate = "", false
				acc.site = n.NodeID()
				acc.resolveDirect(v.field(mdl), "")
				continue
			}
			if spec, selfRepair, agentRung := recoveryRungsOf(n); selfRepair || agentRung {
				// A Verified Action's recovery ladder (#2783): the rungs
				// spend the recovery model — resolved like dispatch's
				// recoveryModel, default included — and each is read the
				// way dispatch runs it.
				seenAnyLLM = true
				spec = recoverySpecOf(v, spec)
				if selfRepair {
					// Rung 3 is a DIRECT generation on the spec (the
					// registry, rung 3 never consults ForNode): the human
					// arm's reading, at the rung's own site.
					acc.backend, acc.backendLate = "", false
					acc.site = n.NodeID() + "#self_repair"
					acc.resolveDirect(spec, "")
				}
				if agentRung {
					// Rung 4 dispatches a synthetic agent node — read it
					// through the SAME shape dispatch builds, so the
					// executor's whole precedence (a kind or glob override
					// reaching `<tool>__recover`, the workflow's default
					// backend) applies exactly as at dispatch.
					syn := recoveryAgentNode(n.NodeID(), spec, nil)
					ov := v.Overrides.ForNode(syn.NodeID(), syn.NodeKind())
					b, fromEnv := resolveRouteBackend(v.field(ov.Backend), "", v.field(wf.DefaultBackend))
					if fromEnv {
						// Expanded with this process's env, not the runner's.
						b = ""
					}
					acc.backend = b
					acc.backendLate = routeBackendLate(v.field(ov.Backend), "", v.field(wf.DefaultBackend))
					acc.site = n.NodeID() + agentRecoverySiteSuffix
					acc.resolveNode(syn, &syn.LLMFields, v.Overrides)
				}
				continue
			}
			if ir.NodeUsesLLM(n) {
				// Spends, but exposes nothing to resolve.
				seenAnyLLM = true
				acc.site = n.NodeID()
				acc.widen()
				acc.unreadableAt(fieldlessSite, "")
			}
			continue
		}
		seenAnyLLM = true
		ov := v.Overrides.ForNode(n.NodeID(), n.NodeKind())
		nodeBackend, fromEnv := resolveRouteBackend(v.field(ov.Backend), v.field(fields.Backend), v.field(wf.DefaultBackend))
		if fromEnv {
			// Expanded with this process's env, not the runner's: unknown for
			// the forfait-first accounting.
			nodeBackend = ""
		}
		nodeBackendLate := routeBackendLate(v.field(ov.Backend), v.field(fields.Backend), v.field(wf.DefaultBackend))
		anyNodeBackendLate = anyNodeBackendLate || nodeBackendLate
		acc.backend, acc.backendLate = nodeBackend, nodeBackendLate
		acc.site = n.NodeID()
		acc.resolveNode(n, fields, v.Overrides)
		// `interaction: llm|llm_or_human` answers the node's questions
		// with a DIRECT generation on `interaction_model` (falling back
		// to the node's model) — a second route the node may spend on.
		// It reads the process's credentials, not the bundle's, so no
		// backend's forfait-first declaration covers it.
		if inf, ok := n.(interface{ GetInteractionFields() *ir.InteractionFields }); ok {
			if f := inf.GetInteractionFields(); f != nil && (f.Interaction == ir.InteractionLLM || f.Interaction == ir.InteractionLLMOrHuman) {
				acc.backend, acc.backendLate = "", false
				acc.site = n.NodeID() + interactionSiteSuffix
				acc.resolveDirect(v.field(f.InteractionModel), v.field(fields.Model))
			}
		}
		// A `fallbacks:` route (ADR-087) is a path the run may take: a
		// primary on claw/openai whose rescue is anthropic MUST be able
		// to spend an anthropic credential, or the route is unreachable.
		if gf, ok := n.(interface{ GetFallbacks() []ir.Fallback }); ok {
			for i, fb := range gf.GetFallbacks() {
				if fb.Action == ir.FallbackActionSkip {
					continue // executes nothing
				}
				// The `when:` gate joins the reading (ADR-129 §4): a route
				// the run's own vars deactivate is not a path the run may
				// take. The view's posture keeps what it cannot answer.
				if !FallbackWhenActive(gateVars, n.NodeID(), fb, nil) {
					continue
				}
				acc.backend, acc.backendLate = nodeBackend, nodeBackendLate
				acc.site = fallbackSite(n.NodeID(), i, fb.Name)
				if strings.TrimSpace(fb.Backend) != "" {
					b, fromEnv := resolveRouteBackend("", v.field(fb.Backend), "")
					if fromEnv {
						b = ""
					}
					acc.backend, acc.backendLate = b, routeBackendLate("", v.field(fb.Backend), "")
				}
				acc.resolveRoute(v.field(fb.Provider), v.field(fb.Model))
			}
		}
	}
	// A supervisor calls its model in process for the whole run
	// (supervise.Bot → Registry.ResolveWithContext): a route like any node's,
	// on no backend that declared a forfait-first precedence — the in-process
	// registry spends a key held for an `anthropic/…` route before the Claude
	// forfait. Without a model of its own it resolves on the runner (the
	// env, the detector, the supervised run's credentials), which the walk
	// cannot name: it widens.
	for _, sup := range wf.Supervisors {
		if sup == nil {
			continue
		}
		seenAnyLLM = true
		acc.backend, acc.backendLate = "", false
		acc.site = "supervisor:" + sup.Name
		acc.resolveDirect(v.field(sup.Model), "")
	}
	if !seenAnyLLM {
		// Nothing spends: nothing to narrow on, and nothing unresolved.
		return ProviderResolution{NarrowSafe: true}
	}
	// The run-level chain (`--fallback` / spec.Fallback / prior.Fallback)
	// lands on every agent node through ir.ApplyRunFallback — the same
	// reasoning as an authored route.
	for i, fb := range v.RunFallbacks {
		if fb.Backend == "" && fb.Model == "" && fb.Provider == "" {
			continue // ApplyRunFallback drops the empty stage too
		}
		// A stage with no backend runs on each node's own; read here, once,
		// it counts as unknown — the conservative side of ForfaitFirst.
		b, fromEnv := resolveRouteBackend("", v.field(fb.Backend), "")
		if fromEnv {
			b = ""
		}
		acc.backend = b
		acc.backendLate = routeBackendLate("", v.field(fb.Backend), "") || (strings.TrimSpace(fb.Backend) == "" && anyNodeBackendLate)
		acc.site = fmt.Sprintf("%s#%d", ir.RunFallbackName, i)
		acc.resolveRoute(v.field(fb.Provider), v.field(fb.Model))
	}
	return acc.result()
}

// fallbackSite names the walk site a route was read at — the key a pin is
// recorded under (ProviderResolution.Pins): the node's id for its own
// route, `node#name` for an authored `fallbacks:` route, its index when the
// author named none.
func fallbackSite(nodeID string, index int, name string) string {
	if strings.TrimSpace(name) != "" {
		return nodeID + "#" + name
	}
	return fmt.Sprintf("%s#%d", nodeID, index)
}

// llmFieldsOf returns the LLMFields a node resolves its route from: agent
// and judge nodes always, a router only in its `llm` mode (its embedded
// fields are empty otherwise, and the deterministic modes spend nothing).
func llmFieldsOf(n ir.Node) (*ir.LLMFields, bool) {
	switch node := n.(type) {
	case *ir.RouterNode:
		if node.RouterMode == ir.RouterLLM {
			return &node.LLMFields, true
		}
		return nil, false
	}
	if f, ok := n.(interface{ GetLLMFields() *ir.LLMFields }); ok {
		return f.GetLLMFields(), true
	}
	return nil, false
}

type providerAccumulator struct {
	known     map[string]bool
	providers map[string]bool
	unknown   map[string]bool
	// envFunded collects the gateway-served prefixes (ProviderResolution.EnvFunded).
	envFunded map[string]bool
	// forfaitFirst[p] stays true while every route naming p ran on a
	// backend that declared forfait-first for p; backend is the route being read.
	forfaitFirst map[string]bool
	backend      string
	// site is where the route being read lives — the key a pin is
	// recorded under (fallbackSite).
	site string
	pins map[string][]string
	// siteProviders is the read-order twin of pins: the known providers
	// each site named, first occurrence first (ProviderResolution.Sites).
	siteProviders map[string][]string
	// vars and late are the walk's vars reading (RouteView.Vars/LateVars);
	// field resolves a routing field against them, and a late name stays
	// as written exactly as a raw `${…}` would.
	vars        map[string]any
	late        map[string]bool
	narrowSafe  bool
	wireDefault map[string]bool
	// envSeen: some route's text defers to an environment or the run's
	// variables (see envDeferred) — the run is never declared env-funded,
	// whatever the expansions said.
	envSeen bool
	// allowed is the allowlist in force (RouteView.AllowedProviders);
	// nil/empty = none, and every branch below records exactly as before.
	allowed          map[string]bool
	forbidden        map[string][]string
	forbiddenPrimary map[string][]string
	unreadable       []UnreadableRoute
	// route is the route being read (beginRoute/endRoute): its outside
	// hints, whether an allowed one served, and the wire-default reads it
	// may commit.
	route *routeRead
	// backendLate: the backend of the route about to be read is decided at
	// dispatch (routeBackendLate) — set beside backend, read by deferAt.
	backendLate bool
	// gatewaySites / deferredSites / unattributedSites:
	// ProviderResolution.GatewaySites, DeferredSites and UnattributedSites.
	gatewaySites      map[string]bool
	deferredSites     map[string]bool
	unattributedSites map[string]bool
	// wireSites: ProviderResolution.WireDefaultSites, per site a slot set.
	wireSites map[string]map[string]bool
	// gatewayKeyFunded is RouteView.GatewayKeyFunded: a decided gateway
	// route spends a sealed key (gatewayKeyed).
	gatewayKeyFunded bool
}

// gatewayKeyed reports whether a DECIDED gateway route records the
// openai_compatible provider (RouteView.GatewayKeyFunded) — only where the
// caller's vocabulary knows that provider: the pool's does not, and there the
// route keeps its env-funded reading rather than becoming a name the walk
// cannot resolve, which would widen the pool's wants to everything.
func (a *providerAccumulator) gatewayKeyed() bool {
	return a.gatewayKeyFunded && a.known[modelroute.OpenAICompatible]
}

// routeRead is one route mid-read: the site it lives at, whether it is a
// PRIMARY (the node's own chain, an interaction generation, a supervisor —
// the class an outside provider refuses the launch for), the providers it
// named outside the allowlist, whether any allowed one served it, and the
// wire-default reads it staged (committed at endRoute only if the route
// survives — a pruned route never runs, so it reads no default).
type routeRead struct {
	site    string
	primary bool
	outside []string
	inside  bool
	wire    []string
}

func (a *providerAccumulator) result() ProviderResolution {
	res := ProviderResolution{NarrowSafe: a.narrowSafe, EnvDeferred: a.envSeen}
	// The widenings that matter here are narrowSafe's (an unresolvable or
	// unknown route may spend anything), a recorded provider (some tier
	// holds what it spends) and an env-dependent route (it may resolve to
	// anything on the runner). An all-openai_compatible walk is none of
	// those: narrow-safe, nameless, unknownless, resolved here.
	res.envFundedOnly = a.narrowSafe && !a.envSeen && len(a.providers) == 0 && len(a.unknown) == 0
	for slot := range a.wireDefault {
		res.AnthropicWireDefaultReads = append(res.AnthropicWireDefaultReads, slot)
	}
	sort.Strings(res.AnthropicWireDefaultReads)
	for p := range a.providers {
		res.Providers = append(res.Providers, p)
	}
	for u := range a.unknown {
		res.Unknown = append(res.Unknown, u)
	}
	for p := range a.envFunded {
		res.EnvFunded = append(res.EnvFunded, p)
	}
	for p, ok := range a.forfaitFirst {
		if ok {
			res.ForfaitFirst = append(res.ForfaitFirst, p)
		}
	}
	for p, sites := range a.pins {
		if res.Pins == nil {
			res.Pins = make(map[string][]string, len(a.pins))
		}
		sorted := append([]string(nil), sites...)
		sort.Strings(sorted)
		deduped := sorted[:0]
		for i, s := range sorted {
			if i == 0 || s != sorted[i-1] {
				deduped = append(deduped, s)
			}
		}
		res.Pins[p] = deduped
	}
	for site, provs := range a.siteProviders {
		if res.Sites == nil {
			res.Sites = make(map[string][]string, len(a.siteProviders))
		}
		res.Sites[site] = append([]string(nil), provs...)
	}
	if len(a.allowed) > 0 {
		res.Forbidden = sortedDedupSites(a.forbidden)
		res.ForbiddenPrimary = sortedDedupSites(a.forbiddenPrimary)
		res.Unreadable = sortDedupUnreadable(a.unreadable)
	}
	res.GatewaySites = sortedSiteSet(a.gatewaySites)
	res.DeferredSites = sortedSiteSet(a.deferredSites)
	res.UnattributedSites = sortedSiteSet(a.unattributedSites)
	for site, slots := range a.wireSites {
		if res.WireDefaultSites == nil {
			res.WireDefaultSites = make(map[string][]string, len(a.wireSites))
		}
		res.WireDefaultSites[site] = append([]string{}, sortedSiteSet(slots)...)
	}
	sort.Strings(res.Providers)
	sort.Strings(res.Unknown)
	sort.Strings(res.EnvFunded)
	sort.Strings(res.ForfaitFirst)
	return res
}

// sortedSiteSet is a site set's sorted keys, nil when empty.
func sortedSiteSet(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for site := range set {
		out = append(out, site)
	}
	sort.Strings(out)
	return out
}

// readLate reports whether a routing field's text is resolved by the
// process that dispatches the route rather than by this reading: an
// environment expansion (`${…}`) the runner performs with its own
// environment, or a `{{…}}` span the view left as written (a late var).
func readLate(v string) bool {
	return strings.Contains(v, "${") || strings.Contains(v, "{{")
}

// routeBackendLate reports whether the backend a route runs on is decided at
// dispatch: the first of override, node and workflow default that would
// decide it (resolveRouteBackend's precedence) — or any source read before
// it that reads the environment, since the runner may expand that one to a
// backend where this process expanded nothing — is read late.
func routeBackendLate(override, node, wfDefault string) bool {
	for _, raw := range []string{override, node, wfDefault} {
		if readLate(raw) {
			return true
		}
		if strings.TrimSpace(raw) != "" {
			return false
		}
	}
	return false
}

// deferAt records the site being read in DeferredSites when the route's
// backend, or any of its deciding fields, is read late.
func (a *providerAccumulator) deferAt(fields ...string) {
	late := a.backendLate
	for _, f := range fields {
		late = late || readLate(f)
	}
	if !late {
		return
	}
	if a.deferredSites == nil {
		a.deferredSites = map[string]bool{}
	}
	a.deferredSites[a.site] = true
}

// gatewayAt records the site being read in GatewaySites.
func (a *providerAccumulator) gatewayAt() {
	if a.gatewaySites == nil {
		a.gatewaySites = map[string]bool{}
	}
	a.gatewaySites[a.site] = true
}

// routeBackend is the backend a route runs on, as far as a launch can read
// it: a launch override, else the node's `backend:` (env-expanded), else the
// workflow default. "" when none names one, or when the first that does is
// "auto" or a `{{vars.…}}` the run resolves at dispatch — a backend nobody
// can name before the run.
func routeBackend(override, node, wfDefault string) string {
	b, _ := resolveRouteBackend(override, node, wfDefault)
	return b
}

// resolveRouteBackend is routeBackend, also reporting whether the backend was
// read from the environment (`${VAR:-…}`). This process expanded it; the
// runner expands it with its own, so a question asked HERE about what the
// runner will spend — the forfait-first accounting — must not take the
// answer on trust.
func resolveRouteBackend(override, node, wfDefault string) (backend string, fromEnv bool) {
	for _, raw := range []string{override, node, wfDefault} {
		// A source that reads the environment counts even when it expanded
		// to nothing here: the runner may expand it to a backend, and then
		// the lower-precedence source this process fell through to is not
		// the one that runs.
		fromEnv = fromEnv || strings.Contains(raw, "${")
		b := strings.TrimSpace(ir.ExpandEnvWithDefault(raw))
		if b == "" {
			continue
		}
		if strings.EqualFold(b, "auto") || strings.Contains(b, "{{") {
			return "", fromEnv
		}
		return strings.ToLower(b), fromEnv
	}
	return "", fromEnv
}

// envDeferred answers for a route whose own text defers to an environment or
// the run's vars. A ${VAR} that expands to NOTHING here is the runner's to
// resolve — unresolved, widen. A ${VAR:-default} (or a VAR the deployment
// sets) resolves deterministically in both processes: narrow-safe. A
// {{vars.…}} reference resolves at dispatch: unresolved, widen. Either way
// the walk is never declared env-funded on an env-deferring route — what the
// runner expands it to is not this process's word.
func (a *providerAccumulator) envDeferred(field string, parts ...string) {
	for _, part := range parts {
		if strings.Contains(part, "{{") {
			a.envSeen = true
			a.widen()
			a.unreadableAt(field, part)
			return
		}
		if strings.Contains(part, "${") {
			a.envSeen = true
			if strings.TrimSpace(ir.ExpandEnvWithDefault(part)) == "" {
				a.widen()
				a.unreadableAt(field, part)
			}
			return
		}
	}
}

// beginRoute opens the route being read (resolveNode: primary,
// resolveRoute: rescue, resolveDirect: primary) — the site and class every
// outside hint and unreadable field is recorded under.
func (a *providerAccumulator) beginRoute(primary bool) {
	a.route = &routeRead{site: a.site, primary: primary}
}

// endRoute closes the route being read. A PRIMARY route that named outside
// providers and no allowed one is unservable: its providers land in
// forbiddenPrimary, the launch refusal (ADR-129 §5: refused, never
// re-routed). The route's staged wire-default reads commit only if it
// survives — an allowlisted route that named only outside providers never
// runs, so it reads no default — and only the slots the allowlist admits:
// a slot outside the list is spent by no route of this run. The SITE commits
// with the route whatever the list leaves of its slots
// (ProviderResolution.WireDefaultSites).
func (a *providerAccumulator) endRoute() {
	r := a.route
	a.route = nil
	if r == nil {
		return
	}
	if r.primary && len(a.allowed) > 0 && !r.inside && len(r.outside) > 0 {
		if a.forbiddenPrimary == nil {
			a.forbiddenPrimary = map[string][]string{}
		}
		for _, p := range r.outside {
			a.forbiddenPrimary[p] = appendSite(a.forbiddenPrimary[p], r.site)
		}
	}
	if len(a.allowed) == 0 || r.inside || len(r.outside) == 0 {
		if len(r.wire) > 0 {
			if a.wireSites == nil {
				a.wireSites = map[string]map[string]bool{}
			}
			if a.wireSites[r.site] == nil {
				a.wireSites[r.site] = map[string]bool{}
			}
		}
		for _, slot := range r.wire {
			// A slot the allowlist excludes is spent by no route of this
			// run — the dispatch filter drops its elements and the seal
			// gate seals nothing for it — so it reads no default either.
			if len(a.allowed) > 0 && !a.allowed[slot] {
				continue
			}
			if a.wireDefault == nil {
				a.wireDefault = map[string]bool{}
			}
			a.wireDefault[slot] = true
			a.wireSites[r.site][slot] = true
		}
	}
}

// widen marks the route being read as one the walk cannot attribute: the run
// may spend anything there (NarrowSafe false), and its site joins
// UnattributedSites — a caller that must refuse such a route names it.
func (a *providerAccumulator) widen() {
	a.narrowSafe = false
	if a.site == "" {
		return
	}
	if a.unattributedSites == nil {
		a.unattributedSites = map[string]bool{}
	}
	a.unattributedSites[a.site] = true
}

// unreadableAt records a route the walk cannot read while an allowlist is
// in force (ADR-129 §5: under a list, readability is compulsory — every
// entry is a launch refusal). Without an allowlist the walk only widens,
// exactly as before.
func (a *providerAccumulator) unreadableAt(field, raw string) {
	if len(a.allowed) == 0 {
		return
	}
	a.unreadable = append(a.unreadable, UnreadableRoute{Site: a.site, Field: field, Hint: raw})
}

// forbid records a route whose resolved provider is OUTSIDE the allowlist:
// excluded from Providers and Pins — no credential is sealed for it, no
// unfunded refusal names it — and named in Forbidden with its site. A
// decided (pruned) route widens nothing.
func (a *providerAccumulator) forbid(provider string) {
	if a.forbidden == nil {
		a.forbidden = map[string][]string{}
	}
	a.forbidden[provider] = appendSite(a.forbidden[provider], a.site)
	if a.route != nil {
		a.route.outside = append(a.route.outside, provider)
	}
}

// appendSite appends a site once (the Pins dedupe, shared).
func appendSite(sites []string, site string) []string {
	for _, s := range sites {
		if s == site {
			return sites
		}
	}
	return append(sites, site)
}

// field resolves a routing field's `{{vars.*}}` spans against the walk's
// vars reading — the same reading RouteView.field gives its callers, so
// the walk and the wire-route readers cannot drift over one templated
// program.
func (a *providerAccumulator) field(raw string) string {
	return resolveRoutingVars(raw, a.vars, a.late)
}

// resolveNode applies the executor's precedence to one LLM node, in the
// executor's order: a launch-time provider override collapses the chain;
// else the DSL `provider:` chain decides — the hint IS the route, and the
// model's `provider/` prefix says nothing about which credential is spent
// (claude_code strips `anthropic/` and, on a `zai` hint, spends ONLY the
// z.ai key; pi's documented z.ai form is `model: "anthropic/glm-5.2"` +
// `provider: "zai"`, the hint overriding the prefix); only a node with no
// hint at all routes on the prefix of its effective model (override, then
// DSL). claw is the exception on the slot: its registry never lets the
// hint name a different credential slot than the prefix's, so a decided
// claw chain records the prefix's slot beside its hints
// (prefixSlotForClaw). Anything the walk cannot resolve widens. Every
// field arrives through the walk's vars reading first: a `{{vars.x}}`
// whose value the run itself decided resolves like the literal it will be
// at dispatch, a late name stays as written.
func (a *providerAccumulator) resolveNode(node ir.Node, fields *ir.LLMFields, overrides ModelOverrides) {
	a.beginRoute(true)
	defer a.endRoute()
	ov := overrides.ForNode(node.NodeID(), node.NodeKind())
	mdl := ov.Model
	if mdl == "" {
		mdl = fields.Model
	}
	prov := a.field(ov.Provider)
	chain := a.field(fields.Provider)
	m := a.field(mdl)
	a.envDeferred("provider", prov)
	a.envDeferred("provider", chain)
	a.envDeferred("model", m)
	// The deciding fields alone: a provider override collapses the chain,
	// so the chain's own text decides nothing then.
	if strings.TrimSpace(ov.Provider) != "" {
		a.deferAt(prov, m)
	} else {
		a.deferAt(chain, m)
	}
	if strings.TrimSpace(ov.Provider) != "" {
		a.noteWireDefault(prov, m)
		if a.chainDecides(prov, m) {
			a.prefixSlotForClaw(m)
			return
		}
		// The override named no resolvable hint ("auto", or a ${VAR} this
		// process cannot read): the runner resolves it with its own env —
		// widen, as hint() on the override did before the gateway
		// classification existed. Falling through would let the node's own
		// chain narrow an answer the override deferred.
		a.widen()
		a.unreadableAt("provider", prov)
		return
	}
	a.noteWireDefault(chain, m)
	if a.chainDecides(chain, m) {
		a.prefixSlotForClaw(m)
		return
	}
	a.prefixOrWiden(m)
}

// prefixSlotForClaw records, on a route whose `provider:` chain just
// decided for backend CLAW, the providers the model's own prefix spends —
// beside the hints the chain recorded. claw's hint decides the ROUTE the
// element dispatch walks, never the SLOT the registry funds: the registry
// reads it at exactly one switch (forcing Anthropic-direct for models
// Anthropic serves) and the credential crossing forwards no hint-named
// slot, so the spend follows the prefix — a GLM id on the anthropic wire
// rides z.ai (prefixOrWiden's reading). Without this second reading the
// walk names only the hint's slot, and the credential plan keys the
// node's spend on a slot the route never funds: every observation joins
// no row (the fine booking lost on every hint-steered claw route, each
// miss voiding the report's table), or — before the event word was
// claw-true — the hint's credential for a prefix spend.
//
// A spec naming no `provider/` prefix reads nothing here. The element
// build hands such a stage the node's baseline model (or the portability
// screen refuses a backend-changing stage whole), and the registry refuses
// a bare claw spec at Execute outright — so the slot question is answered
// by the node's own rows or by no spend at all, never by this reading;
// widening it would nil the plans of runs whose other routes are perfectly
// readable.
func (a *providerAccumulator) prefixSlotForClaw(mdl string) {
	if a.backend != delegate.BackendClaw {
		return
	}
	if providerFromModelPrefix(mdl) == "" {
		return
	}
	a.prefixOrWiden(mdl)
}

// resolveRoute is the fallback-route half, with the same precedence: the
// route's own hint decides; a route without one routes on its model's
// prefix; one that pins neither inherits whatever the process holds,
// which the walk cannot name — widen. Provider and model arrive already
// read through the walk's vars.
func (a *providerAccumulator) resolveRoute(provider, mdl string) {
	a.beginRoute(false)
	defer a.endRoute()
	a.envDeferred("provider", provider)
	a.envDeferred("model", mdl)
	a.deferAt(provider, mdl)
	a.noteWireDefault(provider, mdl)
	if a.chainDecides(provider, mdl) {
		a.prefixSlotForClaw(mdl)
		return
	}
	a.prefixOrWiden(mdl)
}

// resolveDirect is the direct-generation half (an `interaction_model`):
// no hint applies there, the model spec alone routes — its own prefix,
// or the node model's when it is empty.
func (a *providerAccumulator) resolveDirect(interactionModel, nodeModel string) {
	a.beginRoute(true)
	defer a.endRoute()
	mdl := interactionModel
	if strings.TrimSpace(mdl) == "" {
		mdl = nodeModel
	}
	a.envDeferred("model", mdl, nodeModel)
	a.deferAt(mdl)
	a.prefixOrWiden(mdl)
}

// noteWireDefault records the anthropic-wire slots a route may spend as the
// run's default credential (ProviderResolution.AnthropicWireDefaultReads). Not
// called for a direct generation: it reads the process env, or the in-process
// registry, which spends a key its spec names.
func (a *providerAccumulator) noteWireDefault(chain, mdl string) {
	// Read late — from the environment (the runner expands it with its own)
	// or from the run's vars at dispatch: what it will say is unknown here.
	envRead := readLate(chain) || readLate(mdl)
	m := strings.TrimSpace(ir.ExpandEnvWithDefault(mdl))
	glm := GLMOnAnthropicWire(m)
	read := func(slots ...string) {
		if a.route == nil {
			return
		}
		a.route.wire = append(a.route.wire, slots...)
	}
	claudeCode := func() {
		hints, unresolved := chainHints(chain)
		named := len(hints) > 0 && !unresolved && !envRead
		for _, h := range hints {
			named = named && anthropicWireProviders[h]
		}
		switch {
		case named:
		case glm && len(hints) == 0 && !envRead:
			read(string(secrets.ProviderMoonshot), string(secrets.ProviderAnthropic))
		default:
			read(string(secrets.ProviderZAI), string(secrets.ProviderMoonshot), string(secrets.ProviderAnthropic))
		}
	}
	claw := func() {
		if glm && !envRead {
			return
		}
		if p := providerFromModelPrefix(m); envRead || p == "" || p == "anthropic" {
			read(string(secrets.ProviderAnthropic), string(secrets.ProviderZAI))
		}
	}
	switch a.backend {
	case delegate.BackendClaudeCode:
		claudeCode()
	case delegate.BackendClaw:
		claw()
	case "":
		claudeCode()
		claw()
	}
}

// chainDecides records a `provider:` chain's hints and reports whether the
// chain named at least one — in which case it IS the route and the caller
// looks no further. A chain with hints AND an unresolved step ("auto", an
// empty ${VAR}) still decides, and still widens.
//
// mdl is the route's effective model: an openai_compatible hint is only the
// gateway route when the model carries the gateway prefix — a bare model
// behind the hint spends a process-held credential (the backends that do not
// honour the hint), so it widens as an unknown name, whatever the caller's
// vocabulary, and OnlyEnvFunded reads false. A gateway route a sealed key
// funds (gatewayKeyed) records the provider like any vendor hint.
func (a *providerAccumulator) chainDecides(raw, mdl string) bool {
	hints, unresolved := chainHints(raw)
	if len(hints) == 0 {
		return false
	}
	if unresolved {
		a.widen()
		a.unreadableAt("provider", raw)
	}
	for _, h := range hints {
		if h == modelroute.OpenAICompatible {
			switch {
			case !rawGatewayPrefix(mdl):
				a.unknownHint(h)
			case len(a.allowed) > 0 && !a.allowed[modelroute.OpenAICompatible]:
				a.forbid(modelroute.OpenAICompatible)
			case a.gatewayKeyed():
				a.hint(h)
			default:
				a.gatewayAt()
				if a.route != nil {
					// The gateway step is an allowed server: it marks the
					// route servable inside the list, the same way hint()
					// does for a vendor step — a mixed chain refuses only
					// when NO step inside the list holds.
					a.route.inside = true
				}
			}
			continue
		}
		a.hint(h)
	}
	return true
}

// rawGatewayPrefix reports whether the model text, UNEXPANDED, literally
// carries the openai_compatible/ routing prefix. The gateway classification
// answers on this spelling alone: a ${…} model may expand to the gateway in
// this process and to a vendor model on the runner, so an expansion-based
// answer would spend the runner's credential on this process's word.
func rawGatewayPrefix(mdl string) bool {
	prov, _, cut := strings.Cut(strings.TrimSpace(mdl), "/")
	return cut && strings.EqualFold(strings.TrimSpace(prov), modelroute.OpenAICompatible)
}

// prefixOrWiden routes on a model spec's `provider/` prefix, or widens
// when it has none.
func (a *providerAccumulator) prefixOrWiden(mdl string) {
	// A GLM id on the anthropic wire is a z.ai model — claw spells it
	// `anthropic/glm-*`, claude_code takes it bare — so the credential it
	// spends is z.ai's, never the Claude forfait api.anthropic.com would
	// refuse it on.
	if GLMOnAnthropicWire(ir.ExpandEnvWithDefault(mdl)) {
		a.hint("zai")
		return
	}
	if p := providerFromModelPrefix(mdl); p != "" {
		if p == modelroute.OpenAICompatible {
			// The gateway route. Under an allowlist the gateway is a
			// provider like any other: outside the list, the route is
			// pruned. A literal gateway route a sealed key funds
			// (gatewayKeyed, ADR-131 §5) names the gateway's bundle slot,
			// pinned at its site like any provider. Otherwise its
			// credential is the runner's environment: it names no bundle
			// slot and is never a reason to widen (narrowing to the named
			// providers is exact — the route acquires no credential); it is
			// named in EnvFunded for the caller, and it is what
			// OnlyEnvFunded is true OF. An env-TEMPLATED model may expand to
			// the gateway here and to a vendor on the runner, so it widens
			// and is never key-funded (envDeferred covers the empty
			// expansion; this branch covers the resolved one).
			if len(a.allowed) > 0 && !a.allowed[modelroute.OpenAICompatible] {
				a.forbid(modelroute.OpenAICompatible)
				return
			}
			if rawGatewayPrefix(mdl) && a.gatewayKeyed() {
				a.hint(p)
				return
			}
			if a.envFunded == nil {
				a.envFunded = map[string]bool{}
			}
			a.envFunded[p] = true
			a.gatewayAt()
			if !rawGatewayPrefix(mdl) {
				a.widen()
			}
			return
		}
		a.hint(p)
		return
	}
	// A spec that names nothing — no provider hint, no prefix, no GLM
	// family — is the prefix-less model class the readability contract
	// names: the walk cannot say what the route spends, and under an
	// allowlist a lock that cannot be checked must not pass (the run would
	// ride whatever the serving pod's ambient precedence funds).
	a.widen()
	a.unreadableAt("model", mdl)
}

// hint classifies one provider name. Known → recorded (and pinned to the
// site being read — ProviderResolution.Pins), true. Empty or "auto" →
// unresolved, false. Anything else → recorded as Unknown, false.
func (a *providerAccumulator) hint(raw string) bool {
	h := strings.ToLower(strings.TrimSpace(ir.ExpandEnvWithDefault(raw)))
	switch {
	case h == "" || h == "auto":
		a.widen()
		return false
	case a.known[h]:
		if len(a.allowed) > 0 && !a.allowed[h] {
			a.forbid(h)
			return true
		}
		a.providers[h] = true
		a.pin(h)
		a.recordSiteProvider(h)
		ff, seen := a.forfaitFirst[h]
		a.forfaitFirst[h] = (!seen || ff) && delegate.ForfaitFirst(a.backend, h)
		if a.route != nil {
			a.route.inside = true
		}
		return true
	default:
		a.unknownHint(h)
		return false
	}
}

// unknownHint records a name the walk cannot resolve to a provider it may
// attribute: Unknown, the fail-open widen, and — under an allowlist — an
// unreadable route.
func (a *providerAccumulator) unknownHint(h string) {
	a.unknown[h] = true
	a.widen()
	a.unreadableAt("provider", h)
}

// pin records WHERE the provider just recorded was named — the site the
// walk is reading (fallbackSite). Deduplicated; a provider named twice by
// the same node holds the site once.
func (a *providerAccumulator) pin(provider string) {
	if a.site == "" {
		return
	}
	for _, s := range a.pins[provider] {
		if s == a.site {
			return
		}
	}
	a.pins[provider] = append(a.pins[provider], a.site)
}

// recordSiteProvider appends the provider to its site's read-order list —
// ProviderResolution.Sites, the direction the credential plan reads. First
// occurrence wins: the chain order is what survives, not a repeat count.
func (a *providerAccumulator) recordSiteProvider(provider string) {
	if a.site == "" {
		return
	}
	for _, p := range a.siteProviders[a.site] {
		if p == provider {
			return
		}
	}
	if a.siteProviders == nil {
		a.siteProviders = map[string][]string{}
	}
	a.siteProviders[a.site] = append(a.siteProviders[a.site], provider)
}

// chainHints splits a `provider:` field into its lower-cased hints the
// way the executor's resolveProviderChain does: env expansion on the whole
// field first, then `,`, then the `provider:model` step form. Reports
// unresolved=true when the field is empty or any step is "auto" — a step
// that defers to whatever the process holds.
func chainHints(raw string) (hints []string, unresolved bool) {
	expanded := strings.TrimSpace(ir.ExpandEnvWithDefault(raw))
	if expanded == "" {
		return nil, true
	}
	for _, part := range strings.Split(expanded, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue // stray, leading or trailing comma
		}
		hint, _, _ := ir.SplitProviderStep(token)
		hint = strings.ToLower(strings.TrimSpace(hint))
		if hint == "" || hint == "auto" {
			unresolved = true
			continue
		}
		// A `{{vars.…}}` reference is resolved by the executor with the
		// run's vars, which this walk does not have. It still NAMES a step
		// — the hint IS the route, so the model's `provider/` prefix must
		// not decide in its place — but its value is unreadable here: it
		// stays a hint AND marks the chain unresolved, which widens both
		// callers (the wire stays reachable, the credential narrowing
		// fails open). Dropping it instead let the prefix decide, and a
		// cloud run leased no credential for the provider the var named.
		if strings.Contains(hint, "{{") {
			unresolved = true
		}
		hints = append(hints, hint)
	}
	if len(hints) == 0 {
		unresolved = true
	}
	return hints, unresolved
}

// providerFromModelPrefix extracts the provider half of a
// `provider/model` string (env refs expanded), or "" when there is none.
func providerFromModelPrefix(model string) string {
	prov, _, cut := strings.Cut(strings.TrimSpace(ir.ExpandEnvWithDefault(model)), "/")
	// A `{{vars.…}}` prefix resolves at dispatch, with the run's vars this
	// walk does not have: no hint to read, the route defers.
	if !cut || strings.Contains(prov, "{{") {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(prov))
}

// providerOfModelSpec names the provider a model spec spends — the walk's
// own reading (prefixOrWiden): a GLM id on the anthropic wire spends
// z.ai's credential whatever prefix carried it, a `provider/` prefix names
// its provider, and a bare non-GLM id names none ("") — the caller decides
// its posture. The one reading shared by the walk and the dispatch-side
// allowlist decisions, so the two never drift over one spec.
func providerOfModelSpec(spec string) string {
	m := ir.ExpandEnvWithDefault(spec)
	if GLMOnAnthropicWire(m) {
		return string(secrets.ProviderZAI)
	}
	return providerFromModelPrefix(m)
}
