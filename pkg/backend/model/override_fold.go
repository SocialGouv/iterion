package model

import (
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
	// (a human node answering with a model, a subbot), or a fallback
	// route that pins nothing. A caller NARROWING a request on Providers
	// must fail OPEN when it is false — treat the run as able to spend
	// every provider — because a node that resolved to nothing takes
	// whatever credential the process holds (claw substitutes the first
	// available provider; claude_code walks its bundle precedence).
	NarrowSafe bool
	// Unknown lists the hints that named something and matched no known
	// provider — a typo in `provider:`, a model prefix the pool never
	// lends — so the caller can say WHICH pin made it widen. Sorted.
	Unknown []string
	// ForfaitFirst lists the providers of Providers whose EVERY route runs on
	// a backend that spends the provider's forfait before a key pinned for
	// the route (delegate.RegisterForfaitFirst). A positive list: a route on
	// an unknown backend, a direct generation, a run-level fallback with no
	// backend of its own keeps its provider out, and the zero value claims
	// nothing. Sorted.
	ForfaitFirst []string
	// envFundedOnly reports whether every LLM route the walk read resolved
	// to the openai_compatible gateway — a route whose credential the
	// RUNNER's environment funds, never the bundle. A route that resolved
	// to any bundle-funded provider, a route that could not resolve (an
	// "auto", an empty ${VAR}, a name the vocabulary does not know, a
	// model-answering node without fields), and a run with no LLM route at
	// all, all read false.

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
}

// OnlyEnvFunded reports whether the run's every LLM route is an
// openai_compatible route — the gateway the runner's environment funds — so
// the resolution acquires no LLM credential for it. See
// ProviderResolution.envFundedOnly.
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
// `known` is the caller's provider vocabulary. A nil `wf` or an empty
// `known` yields the zero value (NarrowSafe false), which every caller
// reads as "fail open".
func EffectiveProviders(wf *ir.Workflow, overrides ModelOverrides, runFallbacks []FallbackEntry, known map[string]bool) ProviderResolution {
	if wf == nil || len(known) == 0 {
		return ProviderResolution{}
	}
	acc := &providerAccumulator{known: known, providers: map[string]bool{}, unknown: map[string]bool{}, forfaitFirst: map[string]bool{}, narrowSafe: true}
	seenAnyLLM := false
	for _, n := range wf.Nodes {
		fields, ok := llmFieldsOf(n)
		if !ok {
			if ir.NodeUsesLLM(n) {
				// Spends, but exposes nothing to resolve.
				seenAnyLLM = true
				acc.narrowSafe = false
			}
			continue
		}
		seenAnyLLM = true
		ov := overrides.ForNode(n.NodeID(), n.NodeKind())
		nodeBackend, fromEnv := resolveRouteBackend(ov.Backend, fields.Backend, wf.DefaultBackend)
		if fromEnv {
			// Expanded with this process's env, not the runner's: unknown for
			// the forfait-first accounting.
			nodeBackend = ""
		}
		acc.backend = nodeBackend
		acc.resolveNode(n, fields, overrides)
		// `interaction: llm|llm_or_human` answers the node's questions
		// with a DIRECT generation on `interaction_model` (falling back
		// to the node's model) — a second route the node may spend on.
		// It reads the process's credentials, not the bundle's, so no
		// backend's forfait-first declaration covers it.
		if inf, ok := n.(interface{ GetInteractionFields() *ir.InteractionFields }); ok {
			if f := inf.GetInteractionFields(); f != nil && (f.Interaction == ir.InteractionLLM || f.Interaction == ir.InteractionLLMOrHuman) {
				acc.backend = ""
				acc.resolveDirect(f.InteractionModel, fields.Model)
			}
		}
		// A `fallbacks:` route (ADR-087) is a path the run may take: a
		// primary on claw/openai whose rescue is anthropic MUST be able
		// to spend an anthropic credential, or the route is unreachable.
		if gf, ok := n.(interface{ GetFallbacks() []ir.Fallback }); ok {
			for _, fb := range gf.GetFallbacks() {
				if fb.Action == ir.FallbackActionSkip {
					continue // executes nothing
				}
				acc.backend = nodeBackend
				if strings.TrimSpace(fb.Backend) != "" {
					b, fromEnv := resolveRouteBackend("", fb.Backend, "")
					if fromEnv {
						b = ""
					}
					acc.backend = b
				}
				acc.resolveRoute(fb.Provider, fb.Model)
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
		acc.backend = ""
		acc.resolveDirect(sup.Model, "")
	}
	if !seenAnyLLM {
		// Nothing spends: nothing to narrow on, and nothing unresolved.
		return ProviderResolution{NarrowSafe: true}
	}
	sawRoute := true
	// The run-level chain (`--fallback` / spec.Fallback / prior.Fallback)
	// lands on every agent node through ir.ApplyRunFallback — the same
	// reasoning as an authored route.
	for _, fb := range runFallbacks {
		if fb.Backend == "" && fb.Model == "" && fb.Provider == "" {
			continue // ApplyRunFallback drops the empty stage too
		}
		// A stage with no backend runs on each node's own; read here, once,
		// it counts as unknown — the conservative side of ForfaitFirst.
		b, fromEnv := resolveRouteBackend("", fb.Backend, "")
		if fromEnv {
			b = ""
		}
		acc.backend = b
		acc.resolveRoute(fb.Provider, fb.Model)
	}
	return acc.result(sawRoute)
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
	// forfaitFirst[p] stays true while every route naming p ran on a
	// backend declared forfait-first for p; backend is the route being read.
	forfaitFirst map[string]bool
	backend      string
	narrowSafe   bool
	wireDefault  map[string]bool
	// envDependent: some route's text defers to an environment or the run's
	// variables (see envDeferred).
	envDependent bool
}

func (a *providerAccumulator) result(sawRoute bool) ProviderResolution {
	res := ProviderResolution{NarrowSafe: a.narrowSafe}
	// The widenings that matter here are narrowSafe's (an unresolvable or
	// unknown route may spend anything), a recorded provider (some tier
	// holds what it spends) and an env-dependent route (it may resolve to
	// anything on the runner). An all-openai_compatible walk is none of
	// those: narrow-safe, nameless, unknownless, resolved here.
	res.envFundedOnly = sawRoute && a.narrowSafe && !a.envDependent && len(a.providers) == 0 && len(a.unknown) == 0

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
	for p, ok := range a.forfaitFirst {
		if ok {
			res.ForfaitFirst = append(res.ForfaitFirst, p)
		}
	}
	sort.Strings(res.Providers)
	sort.Strings(res.Unknown)
	sort.Strings(res.ForfaitFirst)
	return res
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

// envDeferred marks the walk env-dependent when a route's own text defers to
// an environment or the run's variables: the executor resolves it with the
// runner's env and the run's vars, so what it names is unknown here.
func (a *providerAccumulator) envDeferred(parts ...string) {
	for _, part := range parts {
		if strings.Contains(part, "${") || strings.Contains(part, "{{") {
			a.envDependent = true
			return
		}
	}
}

// resolveNode applies the executor's precedence to one LLM node, in the
// executor's order: a launch-time provider override collapses the chain;
// else the DSL `provider:` chain decides — the hint IS the route, and the
// model's `provider/` prefix says nothing about which credential is spent
// (claude_code strips `anthropic/` and, on a `zai` hint, spends ONLY the
// z.ai key; pi's documented z.ai form is `model: "anthropic/glm-5.2"` +
// `provider: "zai"`, the hint overriding the prefix); only a node with no
// hint at all routes on the prefix of its effective model (override, then
// DSL), the way claw's registry does. Anything the walk cannot resolve
// widens.
func (a *providerAccumulator) resolveNode(node ir.Node, fields *ir.LLMFields, overrides ModelOverrides) {
	ov := overrides.ForNode(node.NodeID(), node.NodeKind())
	mdl := ov.Model
	if mdl == "" {
		mdl = fields.Model
	}
	a.envDeferred(ov.Provider, fields.Provider, mdl)
	if strings.TrimSpace(ov.Provider) != "" {
		a.noteWireDefault(ov.Provider, mdl)
		if a.chainDecides(ov.Provider, mdl) {
			return
		}
		// The launch override collapsed the chain; fall through to the
		// prefix of the effective model.
	}
	a.noteWireDefault(fields.Provider, mdl)
	if a.chainDecides(fields.Provider, mdl) {
		return
	}
	a.prefixOrWiden(mdl)
}

// resolveRoute is the fallback-route half, with the same precedence: the
// route's own hint decides; a route without one routes on its model's
// prefix; one that pins neither inherits whatever the process holds,
// which the walk cannot name — widen.
func (a *providerAccumulator) resolveRoute(provider, mdl string) {
	a.envDeferred(provider, mdl)
	a.noteWireDefault(provider, mdl)
	if a.chainDecides(provider, mdl) {
		return
	}
	a.prefixOrWiden(mdl)
}

// resolveDirect is the direct-generation half (an `interaction_model`):
// no hint applies there, the model spec alone routes — its own prefix,
// or the node model's when it is empty.
func (a *providerAccumulator) resolveDirect(interactionModel, nodeModel string) {
	mdl := interactionModel
	if strings.TrimSpace(mdl) == "" {
		mdl = nodeModel
	}
	a.envDeferred(mdl, nodeModel)
	a.prefixOrWiden(mdl)
}

// noteWireDefault records the anthropic-wire slots a route may spend as the
// run's default credential (ProviderResolution.AnthropicWireDefaultReads). Not
// called for a direct generation: it reads the process env, or the in-process
// registry, which spends a key its spec names.
func (a *providerAccumulator) noteWireDefault(chain, mdl string) {
	// Read late — from the environment (the runner expands it with its own)
	// or from the run's vars at dispatch: what it will say is unknown here.
	late := func(v string) bool { return strings.Contains(v, "${") || strings.Contains(v, "{{") }
	envRead := late(chain) || late(mdl)
	m := strings.TrimSpace(ir.ExpandEnvWithDefault(mdl))
	glm := GLMOnAnthropicWire(m)
	read := func(slots ...string) {
		if a.wireDefault == nil {
			a.wireDefault = map[string]bool{}
		}
		for _, slot := range slots {
			a.wireDefault[slot] = true
		}
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
// honour the hint), so it widens as an unknown name, and OnlyEnvFunded reads
// false.
func (a *providerAccumulator) chainDecides(raw, mdl string) bool {
	hints, unresolved := chainHints(raw)
	if len(hints) == 0 {
		return false
	}
	if unresolved {
		a.narrowSafe = false
	}
	for _, h := range hints {
		if h == modelroute.OpenAICompatible && gatewayRoute(mdl) {
			continue
		}
		a.hint(h)
	}
	return true
}

// gatewayRoute reports whether the route's effective model is
// gateway-prefixed — the only spelling that puts a route on the
// openai_compatible gateway.
func gatewayRoute(mdl string) bool {
	return providerFromModelPrefix(strings.TrimSpace(ir.ExpandEnvWithDefault(mdl))) == modelroute.OpenAICompatible
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
			// The gateway route: its credential is the runner's environment,
			// so it names no bundle slot — recorded nowhere, and never a
			// reason to widen. It is what OnlyEnvFunded is true OF.
			return
		}
		a.hint(p)
		return
	}
	a.narrowSafe = false
}

// hint classifies one provider name. Known → recorded, true. Empty or
// "auto" → unresolved, false. Anything else → recorded as Unknown, false.
func (a *providerAccumulator) hint(raw string) bool {
	h := strings.ToLower(strings.TrimSpace(ir.ExpandEnvWithDefault(raw)))
	switch {
	case h == "" || h == "auto":
		a.narrowSafe = false
		return false
	case a.known[h]:
		a.providers[h] = true
		ff, seen := a.forfaitFirst[h]
		a.forfaitFirst[h] = (!seen || ff) && delegate.ForfaitFirst(a.backend, h)
		return true
	default:
		a.unknown[h] = true
		a.narrowSafe = false
		return false
	}
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
