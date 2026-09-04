package model

import (
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
	// `fallbacks:` route, the run-level chain. Sorted, deduplicated,
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
}

// EffectiveProviders is the walk every "which providers does this run
// actually target?" question goes through — the pool's wants derivation
// and any future site that needs the answer. It reads the DSL under
// launch-time overrides, node `fallbacks:` blocks and the run-level
// fallback chain, resolved like the executor's own resolveProviderChain:
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
	acc := &providerAccumulator{known: known, providers: map[string]bool{}, unknown: map[string]bool{}, narrowSafe: true}
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
		acc.resolveNode(n, fields, overrides)
		// A `fallbacks:` route (ADR-087) is a path the run may take: a
		// primary on claw/openai whose rescue is anthropic MUST be able
		// to spend an anthropic credential, or the route is unreachable.
		if gf, ok := n.(interface{ GetFallbacks() []ir.Fallback }); ok {
			for _, fb := range gf.GetFallbacks() {
				if fb.Action == ir.FallbackActionSkip {
					continue // executes nothing
				}
				acc.resolveRoute(fb.Provider, fb.Model)
			}
		}
	}
	if !seenAnyLLM {
		// Nothing spends: nothing to narrow on, and nothing unresolved.
		return ProviderResolution{NarrowSafe: true}
	}
	// The run-level chain (`--fallback` / spec.Fallback / prior.Fallback)
	// lands on every agent node through ir.ApplyRunFallback — the same
	// reasoning as an authored route.
	for _, fb := range runFallbacks {
		if fb.Backend == "" && fb.Model == "" && fb.Provider == "" {
			continue // ApplyRunFallback drops the empty stage too
		}
		acc.resolveRoute(fb.Provider, fb.Model)
	}
	return acc.result()
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
	known      map[string]bool
	providers  map[string]bool
	unknown    map[string]bool
	narrowSafe bool
}

func (a *providerAccumulator) result() ProviderResolution {
	res := ProviderResolution{NarrowSafe: a.narrowSafe}
	for p := range a.providers {
		res.Providers = append(res.Providers, p)
	}
	for u := range a.unknown {
		res.Unknown = append(res.Unknown, u)
	}
	sort.Strings(res.Providers)
	sort.Strings(res.Unknown)
	return res
}

// resolveNode applies the executor's precedence to one LLM node: a
// provider override collapses the chain; else a `provider/` prefix on the
// effective model (override, then DSL) pins; else the DSL `provider:`
// chain, expanded then split. Anything the walk cannot resolve widens.
func (a *providerAccumulator) resolveNode(node ir.Node, fields *ir.LLMFields, overrides ModelOverrides) {
	ov := overrides.ForNode(node.NodeID(), node.NodeKind())
	if strings.TrimSpace(ov.Provider) != "" {
		a.hint(ov.Provider)
		return
	}
	mdl := ov.Model
	if mdl == "" {
		mdl = fields.Model
	}
	if p := providerFromModelPrefix(mdl); p != "" {
		a.hint(p)
		return
	}
	if !a.chain(fields.Provider) {
		a.narrowSafe = false
	}
}

// resolveRoute is the fallback-route half: a `provider:model` step has
// the same two sources as a node (a hint, or a model prefix). A route
// that pins neither inherits whatever the process holds, which the walk
// cannot name — widen.
func (a *providerAccumulator) resolveRoute(provider, mdl string) {
	if p := providerFromModelPrefix(mdl); p != "" {
		a.hint(p)
		return
	}
	if !a.chain(provider) {
		a.narrowSafe = false
	}
}

// chain resolves a `provider:` field the executor's way. Reports whether
// EVERY step resolved to a known provider (an empty field, an "auto"
// step, an empty ${VAR} or an unknown name all answer false).
func (a *providerAccumulator) chain(raw string) bool {
	expanded := strings.TrimSpace(ir.ExpandEnvWithDefault(raw))
	if expanded == "" {
		return false
	}
	resolved := true
	for _, part := range strings.Split(expanded, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue // stray, leading or trailing comma
		}
		hint, _, _ := ir.SplitProviderStep(token)
		if !a.hint(hint) {
			resolved = false
		}
	}
	return resolved
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
		return true
	default:
		a.unknown[h] = true
		a.narrowSafe = false
		return false
	}
}

// providerFromModelPrefix extracts the provider half of a
// `provider/model` string (env refs expanded), or "" when there is none.
func providerFromModelPrefix(model string) string {
	prov, _, cut := strings.Cut(strings.TrimSpace(ir.ExpandEnvWithDefault(model)), "/")
	if !cut {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(prov))
}
