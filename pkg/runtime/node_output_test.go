package runtime

import "testing"

// `cost.Annotate` writes `_cost_usd` only when a price was resolved, and its
// doc is explicit that a zero there means "no cost data", never "free" — so a
// zero out of extractUsage is "unknown", and SharedBudget.RecordUsage is what
// keeps it out of the enforced axis.
func TestExtractUsage_ZeroCostMeansUnknown(t *testing.T) {
	tests := []struct {
		name       string
		output     map[string]any
		wantTokens int
		wantCost   float64
	}{
		{
			name:       "priced call",
			output:     map[string]any{"_tokens": 1200, "_model": "claude-opus-5", "_cost_usd": 0.42},
			wantTokens: 1200, wantCost: 0.42,
		},
		{
			name:       "tokens but no price for the model",
			output:     map[string]any{"_tokens": 1200, "_model": "gpt-5.6-sol"},
			wantTokens: 1200, wantCost: 0,
		},
		{
			name:       "explicit zero is still unknown, not free",
			output:     map[string]any{"_tokens": 1200, "_cost_usd": 0.0},
			wantTokens: 1200, wantCost: 0,
		},
		{
			name:       "node that made no LLM call",
			output:     map[string]any{"status": "ok"},
			wantTokens: 0, wantCost: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens, cost := extractUsage(tt.output)
			if tokens != tt.wantTokens {
				t.Errorf("tokens = %d, want %d", tokens, tt.wantTokens)
			}
			if cost != tt.wantCost {
				t.Errorf("cost = %v, want %v", cost, tt.wantCost)
			}
		})
	}
}
