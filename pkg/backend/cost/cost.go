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
//  2. The static modelPriceTable below. Acts as the offline fallback
//     for cold starts (no cache file yet) and as a last-known-good
//     for models the live source has not yet published.
//
// Operators can opt out of step 1 with CLAW_DISABLE_LIVE_REGISTRY=1
// (typically in air-gapped environments); the static table then
// serves every lookup.
package cost

import (
	"sort"
	"strings"

	clawapi "github.com/SocialGouv/claw-code-go/pkg/api"
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
// Last reviewed: 2026-04-29. Refresh against vendor pricing pages
// whenever a new model is added or rates are known to change.
type modelPricing struct {
	inputUSDPerMillion  float64
	outputUSDPerMillion float64
}

// Family rates, named once and shared. A missing entry costs more than a
// stale one: an unlisted model reports NO cost at all, and on a stack where
// the spend ceiling is not enforced in flight this table is the only figure
// there is. Naming the rate also states the inheritance — "opus 5 is the opus
// rate" — instead of duplicating a literal that then drifts per line.
var (
	opusRate   = modelPricing{15.00, 75.00}
	sonnetRate = modelPricing{3.00, 15.00}
	haikuRate  = modelPricing{0.25, 1.25}
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
	"claude-opus-4":             opusRate,
	"claude-sonnet-5":           sonnetRate,
	"claude-sonnet-4-7":         sonnetRate,
	"claude-sonnet-4-6":         sonnetRate,
	"claude-sonnet-4-5":         sonnetRate,
	"claude-sonnet-4":           sonnetRate,
	"claude-haiku-4-5":          haikuRate,
	"claude-haiku-4-5-20251001": haikuRate,
	// OpenAI — gpt-5.5+ are priced higher than gpt-5; mini/nano variants
	// are roughly an order of magnitude cheaper. Numbers below are best
	// effort against the known list; refresh against the OpenAI pricing
	// page when a new tier ships.
	"gpt-5":        {1.25, 10.00},
	"gpt-5-mini":   {0.25, 2.00},
	"gpt-5.4":      {1.50, 12.00},
	"gpt-5.4-pro":  {3.00, 25.00},
	"gpt-5.4-mini": {0.30, 2.40},
	"gpt-5.4-nano": {0.15, 1.20},
	"gpt-5.5":      {2.00, 15.00},
	"gpt-5.5-pro":  {4.00, 30.00},
	"gpt-4o":       {2.50, 10.00},
	"gpt-4o-mini":  {0.15, 0.60},
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
	// Fallback: the static table below. Used on cold starts (cache
	// not yet populated) and for any model the live source has not
	// yet shipped.
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	p, ok := modelPriceTable[model]
	if !ok {
		return 0
	}
	return (float64(inputTokens)*p.inputUSDPerMillion + float64(outputTokens)*p.outputUSDPerMillion) / 1_000_000.0
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
