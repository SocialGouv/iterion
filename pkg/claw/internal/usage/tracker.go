// Package usage provides token usage tracking and best-effort cost estimation.
package usage

import (
	"fmt"
	"strings"
)

// ModelPrice holds per-token pricing (in USD per million tokens) for a model.
type ModelPrice struct {
	InputPerMillion  float64
	OutputPerMillion float64
}

// knownPrices maps model IDs to their current pricing.
// Prices are best-effort and may drift; token counts are always authoritative.
var knownPrices = map[string]ModelPrice{
	// Anthropic Claude 4.x
	"claude-opus-4-6":           {InputPerMillion: 15.0, OutputPerMillion: 75.0},
	"claude-sonnet-4-6":         {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-haiku-4-5-20251001": {InputPerMillion: 0.80, OutputPerMillion: 4.0},
	// Anthropic Claude 3.x (legacy)
	"claude-3-5-sonnet-20241022": {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-3-5-haiku-20241022":  {InputPerMillion: 0.80, OutputPerMillion: 4.0},
	"claude-3-opus-20240229":     {InputPerMillion: 15.0, OutputPerMillion: 75.0},
	// OpenAI
	"gpt-4o":      {InputPerMillion: 2.50, OutputPerMillion: 10.0},
	"gpt-4o-mini": {InputPerMillion: 0.15, OutputPerMillion: 0.60},
	"o1-mini":     {InputPerMillion: 3.0, OutputPerMillion: 12.0},
}

// Tracker accumulates token usage across all turns in a session.
type Tracker struct {
	model       string
	TotalInput  int
	TotalOutput int
	CacheWrite  int // prompt cache write tokens (if reported)
	CacheRead   int // prompt cache read tokens (if reported)
	Turns       int // number of completed model turns
}

// NewTracker creates a new Tracker for the given model ID.
func NewTracker(model string) *Tracker {
	return &Tracker{model: model}
}

// Add records token usage for one completed turn.
// cacheWrite and cacheRead may be 0 if the provider does not report them.
func (t *Tracker) Add(input, output, cacheWrite, cacheRead int) {
	t.TotalInput += input
	t.TotalOutput += output
	t.CacheWrite += cacheWrite
	t.CacheRead += cacheRead
	t.Turns++
}

// TotalTokens returns input + output tokens.
func (t *Tracker) TotalTokens() int {
	return t.TotalInput + t.TotalOutput
}

// SetModel updates the model ID (e.g. after a /model switch).
func (t *Tracker) SetModel(model string) {
	t.model = model
}

// CostEstimate returns the estimated USD cost for the session, or -1 if the
// model pricing is unknown.
func (t *Tracker) CostEstimate() float64 {
	price, ok := knownPrices[t.model]
	if !ok {
		return -1
	}
	input := float64(t.TotalInput) * price.InputPerMillion / 1_000_000
	output := float64(t.TotalOutput) * price.OutputPerMillion / 1_000_000
	return input + output
}

// PricingKnown reports whether we have a pricing entry for the current model.
func (t *Tracker) PricingKnown() bool {
	_, ok := knownPrices[t.model]
	return ok
}

// FormatSummary returns a multi-line human-readable usage report.
func (t *Tracker) FormatSummary() string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Model          : %s\n", t.model)
	fmt.Fprintf(&sb, "Turns          : %d\n", t.Turns)
	fmt.Fprintf(&sb, "Input tokens   : %s\n", formatNum(t.TotalInput))
	fmt.Fprintf(&sb, "Output tokens  : %s\n", formatNum(t.TotalOutput))
	fmt.Fprintf(&sb, "Total tokens   : %s\n", formatNum(t.TotalTokens()))

	if t.CacheWrite > 0 || t.CacheRead > 0 {
		fmt.Fprintf(&sb, "Cache write    : %s\n", formatNum(t.CacheWrite))
		fmt.Fprintf(&sb, "Cache read     : %s\n", formatNum(t.CacheRead))
	}

	cost := t.CostEstimate()
	if cost >= 0 {
		fmt.Fprintf(&sb, "Est. cost (USD): $%.6f", cost)
		if cost < 0.001 {
			sb.WriteString(" (<$0.001)")
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("Est. cost      : unavailable (pricing not known for this model/provider)\n")
	}

	return sb.String()
}

// formatNum adds thousands-separator commas to an integer.
func formatNum(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	rem := len(s) % 3
	if rem > 0 {
		out = append(out, s[:rem]...)
	}
	for i := rem; i < len(s); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	return string(out)
}
