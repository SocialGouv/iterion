// Package cost holds the per-model token-pricing table used to annotate
// generation outputs with `_tokens` / `_model` / `_cost_usd`.
//
// It lives in its own leaf package so that both `model/` (claw backend)
// and `delegate/` (claude_code, codex backends) can call `Annotate`
// without creating an import cycle (`model/` already depends on
// `delegate/`).
//
// Pricing resolution order (first match wins):
//
//  1. claw-code-go's live registry cache (refreshed async every 24h
//     from OpenRouter). Picks up new models without iterion-side
//     updates — the long-term path that eliminates the static-table
//     maintenance burden.
//  2. iterion's own model-spec aggregator (models.dev via
//     pkg/backend/modelspecs), whose published cost.input/cost.output
//     were fetched and cached for months before anything read them.
//     Consulted only when BOTH rates are positive — see specRate.
//  3. The static modelPriceTable below. Acts as the offline fallback
//     for cold starts (no cache file yet) and as a last-known-good
//     for models neither live source has published.
//
// Step 1 keeps its precedence over step 2 deliberately: claw already
// answers for a large share of models, and reordering would silently
// change the rate every such run is charged at. Step 2's own argument —
// that models.dev is consensus-filtered across publishers while
// OpenRouter is one of the multi-provider sources that filtering exists
// to neutralize — is real, but it is a pricing decision, and this file's
// standing rule is that a price change is committed by a human, not
// slipped in by a refactor.
//
// Operators can opt out of step 1 with CLAW_DISABLE_LIVE_REGISTRY=1
// (typically in air-gapped environments) and of step 2 with
// ITERION_MODEL_SPECS=off; the static table then serves every lookup.
package cost

import (
	"sort"
	"strings"

	clawapi "github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// pricePerMillion is the per-million-token price (USD) for a small set of
// commonly used models. Two costs per entry: input tokens and output
// tokens. Models not listed return zero, in which case the caller skips
// emitting `_cost_usd` rather than reporting a wrong number.
//
// Keep this table small and conservative. It is only a hint for
// observability — operators concerned with hard budget tracking should
// pull the authoritative rates from their provider invoices.
//
// Last reviewed: 2026-09-06, against models.dev via `iterion models pricing`.
// Run that command to see where this table and the published rates diverge;
// it reports and never rewrites, because a price is a budget decision.
//
// GLM has no entry on purpose, not by oversight: 24 providers publish it at
// rates from 0 to 1.44 per million and the aggregator cannot say which one
// applies. It is nonetheless priced at runtime, because claw's live registry
// resolves it from OpenRouter and is consulted BEFORE this table — a reminder
// that this table is the fallback, not the answer.
type modelPricing struct {
	inputUSDPerMillion  float64
	outputUSDPerMillion float64
}

// Family rates, named once and shared. A missing entry costs more than a
// stale one: an unlisted model reports NO cost at all, and on a stack where
// the spend ceiling is not enforced in flight this table is the only figure
// there is. Naming the rate also states the inheritance — "opus 5 is the opus
// rate" — instead of duplicating a literal that then drifts per line.
// Opus dropped from 15/75 to 5/25 at the 4.5 release; the table missed it and
// kept quoting the old rate for every release since, so a week of runs was
// logged at three times its price. claude-opus-4 legitimately keeps the old
// number — it is the release the drop came after, and its agreeing with the
// aggregator is what made the staleness of the rest legible.
var (
	opusLegacyRate = modelPricing{15.00, 75.00}
	opusRate       = modelPricing{5.00, 25.00}
	sonnetRate     = modelPricing{3.00, 15.00}
	sonnet5Rate    = modelPricing{2.00, 10.00}
	haikuRate      = modelPricing{1.00, 5.00}
	// OpenAI's gpt-5.6 line ships three sizes at three rates (standard tier,
	// developers.openai.com/api/docs/pricing); the bare `gpt-5.6` alias routes
	// to sol, so it carries sol's rate.
	gpt56SolRate   = modelPricing{4.00, 20.00}
	gpt56TerraRate = modelPricing{2.00, 12.00}
	gpt56LunaRate  = modelPricing{0.20, 1.20}
)

var modelPriceTable = map[string]modelPricing{
	// Anthropic — opus / sonnet / haiku families share rates within a
	// family, so newer releases inherit the same numbers until Anthropic
	// publishes a new price.
	"claude-opus-5":             opusRate,
	"claude-opus-4-8":           opusRate,
	"claude-opus-4-7":           opusRate,
	"claude-opus-4-6":           opusRate,
	"claude-opus-4-5":           opusRate,
	"claude-opus-4":             opusLegacyRate,
	"claude-sonnet-5":           sonnet5Rate,
	"claude-sonnet-4-7":         sonnetRate,
	"claude-sonnet-4-6":         sonnetRate,
	"claude-sonnet-4-5":         sonnetRate,
	"claude-sonnet-4":           sonnetRate,
	"claude-haiku-4-5":          haikuRate,
	"claude-haiku-4-5-20251001": haikuRate,
	// OpenAI — each gpt-5.x generation carries its own rate; mini/nano
	// variants are roughly an order of magnitude cheaper. Numbers below are
	// best effort against the known list; refresh against the OpenAI pricing
	// page when a new tier ships.
	"gpt-5":         {1.25, 10.00},
	"gpt-5-mini":    {0.25, 2.00},
	"gpt-5.4":       {2.50, 15.00},
	"gpt-5.4-pro":   {30.00, 180.00},
	"gpt-5.4-mini":  {0.75, 4.50},
	"gpt-5.4-nano":  {0.20, 1.25},
	"gpt-5.5":       {5.00, 30.00},
	"gpt-5.5-pro":   {30.00, 180.00},
	"gpt-5.6":       gpt56SolRate,
	"gpt-5.6-sol":   gpt56SolRate,
	"gpt-5.6-terra": gpt56TerraRate,
	"gpt-5.6-luna":  gpt56LunaRate,
	"o3":            {2.00, 8.00},
	"gpt-4o":        {2.50, 10.00},
	"gpt-4o-mini":   {0.15, 0.60},
}

// EstimateUSD returns a rough cost estimate for the given token usage on
// the named model. Returns 0 when the model is not in the price table —
// callers should treat 0 as "unknown" and skip emission.
//
// The model parameter accepts both bare IDs ("claude-sonnet-4-6") and
// fully qualified specs ("anthropic/claude-sonnet-4-6"); only the
// trailing path component is consulted. This means a region- or
// tenant-prefixed spec like "anthropic/eu/claude-sonnet-4-6" still
// resolves to "claude-sonnet-4-6" — intentional, since pricing is the
// same across regions for the providers we track.
func EstimateUSD(model string, inputTokens, outputTokens int) float64 {
	// First: ask claw's live registry cache. When it has a hit, it
	// reflects the OpenRouter pricing as published — which means new
	// models picked up since the last static-table update get correct
	// estimates without anyone editing this file.
	if pricing, ok := clawapi.LookupModelPricing(model); ok {
		return (float64(inputTokens)*pricing.InputUSDPerMillion + float64(outputTokens)*pricing.OutputUSDPerMillion) / 1_000_000.0
	}
	// The model string arrives in every shape a backend might report:
	// bare ("claude-opus-5"), qualified ("anthropic/claude-opus-5"),
	// region-prefixed ("anthropic/eu/claude-opus-5") and CLI-backend
	// forms ("kimi-code/kimi-for-coding"). Only the trailing component
	// is a model id, and pricing is the same across regions for the
	// providers tracked here.
	prefix, bare := splitModelSpec(model)
	// Second: iterion's own aggregator. It has carried published rates in
	// its cache since ADR-042 while this function priced from a table
	// alone — so a model models.dev knew the price of could still report
	// no cost at all.
	if in, out, ok := specRate(prefix, bare); ok {
		return (float64(inputTokens)*in + float64(outputTokens)*out) / 1_000_000.0
	}
	model = bare
	// Fallback: the static table below. Used on cold starts (no cache
	// populated) and for any model neither live source has shipped.
	p, ok := modelPriceTable[model]
	if !ok {
		return 0
	}
	return (float64(inputTokens)*p.inputUSDPerMillion + float64(outputTokens)*p.outputUSDPerMillion) / 1_000_000.0
}

// splitModelSpec separates a model string's leading component from its model
// id. The leading component is whatever the caller put there — a provider
// ("anthropic/claude-opus-5"), a backend ("kimi-code/kimi-for-coding"), or a
// provider plus a region ("anthropic/eu/claude-opus-5"). It is passed to the
// registry as a provider HINT, never as a claim: the registry falls back to
// its bare index when the qualified key misses, so a backend name in that
// position costs nothing.
func splitModelSpec(model string) (prefix, bare string) {
	i := strings.LastIndex(model, "/")
	if i < 0 {
		return "", model
	}
	return model[:i], model[i+1:]
}

// specLookup is the aggregator seam, a package var so tests swap in a fixture
// instead of resolving against whatever the host's ~/.iterion cache last
// fetched. Production wiring is the process-wide registry.
//
// It goes through the QUALIFIED lookup, which tries "provider/model" before
// the bare index. That ordering is what keeps the tier useful on the models
// most likely to be run: the bare index is consensus-filtered, so it reports
// UNKNOWN as soon as two publishers quote different rates — while the
// provider's own entry holds the right number. Resolving claude-opus-5 by the
// bare index alone would drop it to the static table the moment a second
// publisher listed it.
var specLookup = func(provider, bareModel string) (modelspecs.Spec, bool) {
	return modelspecs.Default().Lookup(provider, bareModel)
}

// specRate returns the aggregator's published pair for a model, and whether it
// is usable.
//
// USABLE MEANS BOTH RATES POSITIVE. The two are published and
// consensus-filtered independently, so a half-known pair is routine, and taking
// it would price the missing half at zero — the exact shape of "looks right, is
// wrong" this file already shipped once, when an unlisted model reported no
// cost while a run burned real money. A half-published pair therefore falls
// through WHOLE to the static table: mixing one rate from the aggregator with
// the other from the table would produce a figure traceable to neither source.
func specRate(provider, bareModel string) (inputPerM, outputPerM float64, ok bool) {
	spec, found := specLookup(provider, bareModel)
	if !found || spec.InputCostPerM <= 0 || spec.OutputCostPerM <= 0 {
		return 0, 0, false
	}
	return spec.InputCostPerM, spec.OutputCostPerM, true
}

// Annotate writes the conventional `_tokens` / `_model` / `_cost_usd`
// keys onto a generation output. Cost is omitted when the model is
// unknown to the price table, so observers can distinguish "no cost
// data" from "$0". A nil output map is a no-op (returns 0).
func Annotate(output map[string]any, model string, inputTokens, outputTokens int) (totalTokens int) {
	totalTokens = inputTokens + outputTokens
	if output == nil {
		return totalTokens
	}
	output["_tokens"] = totalTokens
	output["_model"] = model
	if cost := EstimateUSD(model, inputTokens, outputTokens); cost > 0 {
		output["_cost_usd"] = cost
	}
	return totalTokens
}

// USDFromOutput reads back the `_cost_usd` key Annotate / AnnotateWithUSD
// wrote onto a generation output. Returns 0 when the key is absent — which
// this package deliberately uses to mean "no cost data" (unknown model)
// rather than "$0", so callers must treat a zero as unknown and not as a
// measured free call.
func USDFromOutput(output map[string]any) float64 {
	if output == nil {
		return 0
	}
	switch v := output["_cost_usd"].(type) {
	case float64:
		if v > 0 {
			return v
		}
	case int:
		if v > 0 {
			return float64(v)
		}
	case int64:
		if v > 0 {
			return float64(v)
		}
	}
	return 0
}

// StaticRate returns the committed fallback rate for a model, and whether the
// table carries one at all. Exported so the pricing audit can compare what is
// committed against what the spec aggregator publishes: the two silently
// disagreed for months because nothing could see both at once.
func StaticRate(model string) (inputPerM, outputPerM float64, ok bool) {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	p, ok := modelPriceTable[model]
	if !ok {
		return 0, 0, false
	}
	return p.inputUSDPerMillion, p.outputUSDPerMillion, true
}

// StaticTableModels returns the model keys the committed table covers, sorted.
// The audit iterates these to report an entry the aggregator no longer knows
// about, which is how a renamed or retired model is spotted.
func StaticTableModels() []string {
	out := make([]string, 0, len(modelPriceTable))
	for k := range modelPriceTable {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// EffectiveRate reports the rate a run would actually be charged at — that is,
// what EstimateUSD resolves after consulting the live registry and then this
// table. The audit needs this and not just the table: a first version compared
// the committed table against the aggregator alone and declared GLM unpriced,
// while the live registry was pricing it all along. Reporting on a source the
// estimator does not use produces confident, false verdicts.
func EffectiveRate(model string) (inputPerM, outputPerM float64, ok bool) {
	inputPerM = EstimateUSD(model, 1_000_000, 0)
	outputPerM = EstimateUSD(model, 0, 1_000_000)
	return inputPerM, outputPerM, inputPerM > 0 || outputPerM > 0
}

// AnnotateWithUSD is Annotate for backends whose CLI reports a cost computed
// by the provider itself (e.g. pi's AssistantMessage.usage.cost.total).
//
// When costUSD is positive it wins over EstimateUSD: an authoritative
// number from the provider beats our static price table plus OpenRouter
// cache — it accounts for the real cache-read/cache-write split, per-tenant
// pricing, and models the table has never heard of. When costUSD is zero
// (the CLI reports none) the call degrades exactly to Annotate.
func AnnotateWithUSD(output map[string]any, model string, inputTokens, outputTokens int, costUSD float64) (totalTokens int) {
	if costUSD <= 0 {
		return Annotate(output, model, inputTokens, outputTokens)
	}
	totalTokens = inputTokens + outputTokens
	if output == nil {
		return totalTokens
	}
	output["_tokens"] = totalTokens
	output["_model"] = model
	output["_cost_usd"] = costUSD
	return totalTokens
}
