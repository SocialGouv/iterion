// Package modelcatalog crosses the four sources that together answer "which
// models can I actually use here":
//
//	model.KnownModelSpecs()  — what iterion knows about (curated + aggregator)
//	detect.Report            — which credentials this host really holds
//	llmtypes.ModelCapabilities — context window, tool-calling, reasoning
//	cost.EffectiveRate       — the price a run would actually be charged at
//
// It is the single code path behind both `iterion models` and
// GET /api/models, so the CLI and the studio can never disagree about what a
// model is or whether it is reachable.
//
// The package is deliberately free of any bot/workflow knowledge: it answers
// about MODELS, never about who is going to use one.
package modelcatalog

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// Entry is one model in the catalog: what it is, what it can do, whether this
// host can reach it right now, and roughly what it costs.
type Entry struct {
	// Spec is the canonical "provider/model-id" string a DSL `model:` field
	// or a `model_overrides` entry takes.
	Spec string `json:"spec"`
	// Provider is the spec's provider prefix (the API dialect: "anthropic",
	// "openai", …) — NOT necessarily the vendor holding the credential.
	Provider string `json:"provider"`
	// Model is the bare model id.
	Model string `json:"model"`
	// CredentialProvider is the detect provider whose credential unlocks this
	// spec. It differs from Provider for façade endpoints — the GLM family is
	// served over the Anthropic-compatible API, so `anthropic/glm-5.2` needs a
	// "zai" credential, not an Anthropic one.
	CredentialProvider string `json:"credential_provider"`
	// Source is where the capability values came from: "aggregator" (models.dev)
	// or "curated" (the static fallback table).
	Source string `json:"source"`

	ContextWindow int  `json:"context_window"`
	Reasoning     bool `json:"reasoning"`
	// ToolCall is the hard capability gate: a model without it cannot drive
	// board tools, skills, or run introspection, so an agent node on it is
	// broken rather than degraded.
	ToolCall    bool `json:"tool_call"`
	Temperature bool `json:"temperature"`
	// UltracodeCapable reports whether `reasoning_effort: ultracode` holds on
	// this model. Ultracode's workflow-orchestration prerogative rides
	// Anthropic mid-conversation system messages and is reliable only on
	// claude-opus-4-8 (diagnostic C089) — elsewhere it silently degrades to
	// plain xhigh, which is exactly the quiet downgrade a picker must name.
	UltracodeCapable bool `json:"ultracode_capable"`

	// InputCostPerM / OutputCostPerM are the per-million-token USD rates a run
	// would actually be charged at (cost.EffectiveRate: the committed table
	// wins, else the aggregator). Zero means unknown — never free.
	InputCostPerM  float64 `json:"input_cost_per_m,omitempty"`
	OutputCostPerM float64 `json:"output_cost_per_m,omitempty"`
	// PriceKnown disambiguates "costs nothing" from "we have no price", which
	// a bare zero cannot.
	PriceKnown bool `json:"price_known"`

	// Usable is true when at least one detected backend can drive this spec
	// with the credentials present on this host right now.
	Usable bool `json:"usable"`
	// UnusableReason explains a false Usable in operator language.
	UnusableReason string `json:"unusable_reason,omitempty"`
	// Backends lists the detected backends that can drive this spec, in
	// preference order. Empty when Usable is false.
	Backends []string `json:"backends,omitempty"`
	// CredentialSource names the credential that would be used (e.g.
	// "ANTHROPIC_API_KEY"), mirroring detect.ProviderStatus.Source.
	CredentialSource string `json:"credential_source,omitempty"`
	// Recommended marks the spec the host's own detection suggests — the
	// "keep the good default one click away" affordance.
	Recommended bool `json:"recommended,omitempty"`
}

// Catalog is the full answer: every known model plus the host-level context a
// caller needs to render a picker.
type Catalog struct {
	Models []Entry `json:"models"`
	// RecommendedSpec is the spec Recommended marks, or "" when this host has
	// no credential at all.
	RecommendedSpec string `json:"recommended_spec,omitempty"`
	// ResolvedDefaultBackend mirrors detect.Report.ResolvedDefault: the backend
	// a node with no explicit `backend:` would resolve to.
	ResolvedDefaultBackend string `json:"resolved_default_backend,omitempty"`
	// Backends lists every backend detection considered, available or not, so
	// a picker can offer a backend override without a second round-trip.
	Backends []detect.BackendStatus `json:"backends,omitempty"`
	// Refreshed / RefreshError report an explicit aggregator refresh. A failed
	// refresh is never fatal: the cached/curated values still answer.
	Refreshed    bool   `json:"refreshed,omitempty"`
	RefreshError string `json:"refresh_error,omitempty"`
	// InvalidSpecs lists ExtraSpecs that could not be resolved, with the
	// reason. They are SKIPPED rather than fatal: a picker asks about every
	// node's DSL default at once, and one bot pinning a malformed `model:`
	// must not blank out the whole catalog for every other model on the host.
	InvalidSpecs []InvalidSpec `json:"invalid_specs,omitempty"`
}

// InvalidSpec is one caller-supplied hint the catalog could not resolve.
type InvalidSpec struct {
	Spec   string `json:"spec"`
	Reason string `json:"reason"`
}

// Options configures Build.
type Options struct {
	// Specs REPLACES the model set: the catalog carries exactly these and
	// nothing else. Empty means model.KnownModelSpecs(). This is
	// `iterion models <spec>` — "tell me about this one".
	Specs []string
	// ExtraSpecs ADDS to the set, deduped and order-preserved. This is what a
	// picker wants: the curated set PLUS whatever the bot's own nodes pin,
	// which may sit outside it. Passing them as Specs instead would narrow the
	// picker to the models already in use — the one list from which no new
	// choice can be made.
	ExtraSpecs []string
	// Refresh force-refetches the model-spec aggregator cache first.
	Refresh bool
	// Report is the host credential snapshot. When nil, Build calls
	// detect.Detect itself — pass a cached report on hot paths.
	Report *detect.Report
}

// Build resolves the catalog. It returns an error only when a caller-supplied
// spec is malformed; every other degradation (offline aggregator, no
// credentials) is reported in the result rather than raised.
func Build(ctx context.Context, opts Options) (Catalog, error) {
	var cat Catalog

	if opts.Refresh {
		cat.Refreshed = true
		if err := model.RefreshModelSpecs(ctx); err != nil {
			cat.RefreshError = err.Error()
		}
	}

	report := opts.Report
	if report == nil {
		r := detect.Detect(ctx)
		report = &r
	}
	cat.ResolvedDefaultBackend = report.ResolvedDefault
	cat.Backends = report.Backends

	// A requested spec is the caller's own question ("tell me about THIS
	// one") and a malformed one is a user error worth raising. An extra spec
	// is a HINT harvested from elsewhere — a bot's DSL default, which the
	// picker collects across every node — so one bad hint degrades to a
	// reported skip instead of erasing the answer for every valid model.
	requested := opts.Specs
	if len(requested) == 0 {
		requested = model.KnownModelSpecs()
	}
	specs := make([]struct {
		spec     string
		optional bool
	}, 0, len(requested)+len(opts.ExtraSpecs))
	for _, s := range requested {
		specs = append(specs, struct {
			spec     string
			optional bool
		}{s, false})
	}
	for _, s := range opts.ExtraSpecs {
		specs = append(specs, struct {
			spec     string
			optional bool
		}{s, true})
	}
	recommended := recommendedSpec(*report)
	// The host's own recommendation is not necessarily inside
	// KnownModelSpecs — detect advertises `xai/grok-3` for the xai provider
	// and the curated list has no xai row — so computing it and then
	// dropping it left a fully credentialed host looking at a catalog where
	// every model reads "unusable" and nothing is one click away. Add it as
	// an optional entry: unresolvable becomes a reported skip, never fatal.
	//
	// Skipped when the caller named its own Specs: `iterion models <spec>`
	// asks about exactly one model and must not grow a second row.
	if recommended != "" && len(opts.Specs) == 0 {
		specs = append(specs, struct {
			spec     string
			optional bool
		}{recommended, true})
	}

	seen := make(map[string]bool, len(specs))
	for _, entry := range specs {
		spec := strings.TrimSpace(entry.spec)
		if spec == "" || seen[spec] {
			continue
		}
		seen[spec] = true
		rc, err := model.ResolveSpec(spec)
		if err != nil {
			if entry.optional {
				cat.InvalidSpecs = append(cat.InvalidSpecs, InvalidSpec{Spec: spec, Reason: err.Error()})
				continue
			}
			return Catalog{}, err
		}
		e := Entry{
			Spec:               rc.Spec,
			Provider:           rc.Provider,
			Model:              rc.Model,
			CredentialProvider: CredentialProviderFor(rc.Provider, rc.Model),
			Source:             string(rc.Source),
			ContextWindow:      rc.ContextWindow,
			Reasoning:          rc.Reasoning,
			ToolCall:           rc.ToolCall,
			Temperature:        rc.Temperature,
			UltracodeCapable:   UltracodeCapable(rc.Spec),
			Recommended:        rc.Spec == recommended,
		}
		if in, out, ok := cost.EffectiveRate(rc.Spec); ok {
			e.InputCostPerM, e.OutputCostPerM, e.PriceKnown = in, out, true
		}
		e.Backends, e.CredentialSource, e.UnusableReason = availability(*report, e.Provider, e.Model, e.CredentialProvider)
		e.Usable = len(e.Backends) > 0
		cat.Models = append(cat.Models, e)
	}

	// Only claim a recommendation the catalog actually contains, so a client
	// can always resolve RecommendedSpec against a row it rendered.
	for _, m := range cat.Models {
		if m.Recommended {
			cat.RecommendedSpec = m.Spec
			break
		}
	}
	return cat, nil
}

// CredentialProviderFor maps a spec's API dialect to the vendor whose
// credential actually unlocks it.
//
// The GLM family is the case that forces this to exist: it speaks the
// Anthropic API, so its specs read `anthropic/glm-*`, but the credential is
// z.ai's. Treating the prefix as the vendor reported GLM usable on any host
// holding an ANTHROPIC_API_KEY, which 401s.
func CredentialProviderFor(specProvider, modelID string) string {
	if strings.EqualFold(specProvider, "anthropic") &&
		strings.HasPrefix(strings.ToLower(modelID), "glm") {
		return "zai"
	}
	return strings.ToLower(specProvider)
}

// UltracodeCapable reports whether `reasoning_effort: ultracode` holds on this
// spec. It mirrors the ir compiler's C089 gate (modelIsOpus48) — kept as a
// separate, tiny predicate rather than an import so the catalog does not drag
// the DSL compiler into the HTTP layer.
func UltracodeCapable(spec string) bool {
	m := strings.ToLower(strings.TrimSpace(spec))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return m == "opus" || strings.Contains(m, "opus-4-8")
}

// availability answers "can this host drive this spec right now", returning the
// backends that can, the credential that would be used, and — when none can —
// why not.
func availability(report detect.Report, specProvider, modelID, credProvider string) (backends []string, credSource, reason string) {
	prov, known := providerStatus(report, credProvider)
	if known {
		credSource = prov.Source
	}

	claudeCodeFits := strings.EqualFold(specProvider, "anthropic") &&
		strings.HasPrefix(strings.ToLower(modelID), "claude")

	for _, name := range backendOrder(report) {
		b, ok := backendStatus(report, name)
		if !ok || !b.Available {
			continue
		}
		switch name {
		case detect.BackendClaudeCode:
			// claude_code is an Anthropic-Claude agent: it carries its own
			// OAuth credential and cannot be pointed at another vendor.
			if claudeCodeFits {
				backends = append(backends, name)
			}
		case detect.BackendClaw, detect.BackendPi:
			// Both resolve a provider credential from the environment, so
			// they can drive whatever the detected provider unlocks.
			if prov.Available {
				backends = append(backends, name)
			}
		}
		// codex is deliberately absent: deprecated (C030), never auto-selected.
	}

	if len(backends) > 0 {
		return backends, credSource, ""
	}
	switch {
	case !known:
		reason = fmt.Sprintf("iterion has no credential probe for provider %q", credProvider)
	case !prov.Available && claudeCodeFits:
		reason = "no Anthropic credential and no signed-in Claude Code CLI on this host"
	case !prov.Available:
		reason = fmt.Sprintf("no credential detected for provider %q", credProvider)
	default:
		reason = fmt.Sprintf("credential for %q is present but no backend able to drive it is available", credProvider)
	}
	return nil, credSource, reason
}

// backendOrder returns the host's backend preference order, with any detected
// backend the preference does not name appended so nothing is silently hidden.
func backendOrder(report detect.Report) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(report.Backends)+len(report.PreferenceOrder))
	for _, name := range report.PreferenceOrder {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, b := range report.Backends {
		if !seen[b.Name] {
			seen[b.Name] = true
			out = append(out, b.Name)
		}
	}
	return out
}

func providerStatus(report detect.Report, name string) (detect.ProviderStatus, bool) {
	for _, p := range report.Providers {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return detect.ProviderStatus{}, false
}

func backendStatus(report detect.Report, name string) (detect.BackendStatus, bool) {
	for _, b := range report.Backends {
		if b.Name == name {
			return b, true
		}
	}
	return detect.BackendStatus{}, false
}

// recommendedSpec picks the spec this host should default to: the suggested
// model of the first available provider, in the host's backend preference
// order where that is meaningful, else the first available provider at all.
//
// A signed-in claude_code CLI counts as an Anthropic credential even with no
// ANTHROPIC_API_KEY — that is the single most common local setup, and leaving
// it unrecommended would surface "no recommendation" on a perfectly working host.
func recommendedSpec(report detect.Report) string {
	if b, ok := backendStatus(report, detect.BackendClaudeCode); ok && b.Available {
		if p, ok := providerStatus(report, "anthropic"); ok && p.SuggestedModel != "" {
			return p.SuggestedModel
		}
	}
	for _, p := range report.Providers {
		if p.Available && p.SuggestedModel != "" {
			return p.SuggestedModel
		}
	}
	return ""
}

// SortedSpecs returns the catalog's specs in stable order — handy for tests and
// for any caller that wants a deterministic list without re-sorting entries.
func (c Catalog) SortedSpecs() []string {
	out := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		out = append(out, m.Spec)
	}
	sort.Strings(out)
	return out
}

// Find returns the entry for a spec, if the catalog carries one.
func (c Catalog) Find(spec string) (Entry, bool) {
	for _, m := range c.Models {
		if m.Spec == spec {
			return m, true
		}
	}
	return Entry{}, false
}
