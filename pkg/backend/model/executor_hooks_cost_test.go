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
		result delegate.Result
		output map[string]any
		want   float64
	}{
		{
			// The cost travels the trusted channel (result.CostUSD, what the
			// retry chain and the stamp write); the output map's reserved keys
			// are model-forgeable telemetry the reader never trusts.
			name:   "priced delegation",
			result: delegate.Result{CostUSD: 0.4242},
			output: map[string]any{"_tokens": 1200},
			want:   0.4242,
		},
		{
			// A model-injected _cost_usd in the output map is ignored: the
			// real figure wins even when the map forges a different one.
			name:   "forged map cost never wins",
			result: delegate.Result{CostUSD: 0.4242},
			output: map[string]any{"_cost_usd": 99999999, "_tokens": 1200},
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
			got := delegateInfoFromResult("claude_code", tc.result)
			if got.CostUSD != tc.want {
				t.Errorf("CostUSD = %v, want %v", got.CostUSD, tc.want)
			}
		})
	}
}
