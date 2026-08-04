package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// A delegation's cost lives on the result output as `_cost_usd` — the CLI's
// own figure when it reports one, else the token estimate a subscription
// session falls back to. DelegateInfo must carry it, because the hook payload
// is the only path by which a delegate backend's spend reaches the runner's
// per-run totals (and from there the org cost cap and the credential pool).
func TestDelegateInfoFromResult_carriesCost(t *testing.T) {
	cases := []struct {
		name   string
		output map[string]any
		want   float64
	}{
		{
			name:   "priced delegation",
			output: map[string]any{"_cost_usd": 0.4242, "_tokens": 1200},
			want:   0.4242,
		},
		{
			// Annotate omits the key when the price table doesn't know the
			// model. That must read back as "no data" (0), never as a
			// measured free call.
			name:   "unpriced model omits the key",
			output: map[string]any{"_tokens": 1200},
			want:   0,
		},
		{
			name:   "no output at all",
			output: nil,
			want:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := delegateInfoFromResult("claude_code", delegate.Result{Output: tc.output})
			if got.CostUSD != tc.want {
				t.Errorf("CostUSD = %v, want %v", got.CostUSD, tc.want)
			}
		})
	}
}
