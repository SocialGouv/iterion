// Package cost holds the per-model token-pricing table used to annotate
// generation outputs with `_tokens` / `_model` / `_cost_usd`.
//
// It lives in its own leaf package so that both `model/` (claw backend)
// and `delegate/` (claude_code, codex backends) can call `Annotate`
// without creating an import cycle (`model/` already depends on
// `delegate/`).
package cost

import "strings"

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

var modelPriceTable = map[string]modelPricing{
	// Anthropic
	"claude-opus-4-7":           {15.00, 75.00},
	"claude-opus-4-5":           {15.00, 75.00},
	"claude-opus-4":             {15.00, 75.00},
	"claude-sonnet-4-6":         {3.00, 15.00},
	"claude-sonnet-4-5":         {3.00, 15.00},
	"claude-sonnet-4":           {3.00, 15.00},
	"claude-haiku-4-5":          {0.25, 1.25},
	"claude-haiku-4-5-20251001": {0.25, 1.25},
	// OpenAI
	"gpt-5":       {1.25, 10.00},
	"gpt-5-mini":  {0.25, 2.00},
	"gpt-4o":      {2.50, 10.00},
	"gpt-4o-mini": {0.15, 0.60},
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
func Annotate(output map[string]interface{}, model string, inputTokens, outputTokens int) (totalTokens int) {
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
