package delegate

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
)

func fptr(v float64) *float64 { return &v }

// annotateCost must price with the CLI-resolved effective model when the
// node declares none (backend auto-detection), prefer a CLI-computed cost
// over the static estimate, and still record tokens when no price is
// resolvable at all.
func TestAnnotateCost(t *testing.T) {
	tests := []struct {
		name      string
		taskModel string
		effective string
		rms       []*claudesdk.ResultMessage
		wantModel string
		wantCost  func(t *testing.T, cost any)
	}{
		{
			name:      "empty task model falls back to effective model for pricing",
			taskModel: "",
			effective: "claude-opus-5",
			wantModel: "claude-opus-5",
			wantCost: func(t *testing.T, cost any) {
				c, ok := cost.(float64)
				if !ok || c <= 0 {
					t.Fatalf("_cost_usd = %v, want a positive estimate for claude-opus-5", cost)
				}
			},
		},
		{
			name:      "declared task model wins over effective",
			taskModel: "claude-sonnet-4-6",
			effective: "claude-opus-5",
			wantModel: "claude-sonnet-4-6",
			wantCost: func(t *testing.T, cost any) {
				if c, ok := cost.(float64); !ok || c <= 0 {
					t.Fatalf("_cost_usd = %v, want a positive estimate", cost)
				}
			},
		},
		{
			name:      "CLI-computed cost wins over the estimate",
			taskModel: "",
			effective: "claude-opus-5",
			rms:       []*claudesdk.ResultMessage{{TotalCostUSD: fptr(0.42)}},
			wantModel: "claude-opus-5",
			wantCost: func(t *testing.T, cost any) {
				if c, ok := cost.(float64); !ok || c != 0.42 {
					t.Fatalf("_cost_usd = %v, want the CLI-reported 0.42", cost)
				}
			},
		},
		{
			name:      "max across result messages, never the sum",
			taskModel: "",
			effective: "claude-opus-5",
			rms: []*claudesdk.ResultMessage{
				{TotalCostUSD: fptr(0.30)},
				{TotalCostUSD: fptr(0.35)}, // session-cumulative: subsumes the first
			},
			wantModel: "claude-opus-5",
			wantCost: func(t *testing.T, cost any) {
				if c, ok := cost.(float64); !ok || c != 0.35 {
					t.Fatalf("_cost_usd = %v, want max 0.35 (not the 0.65 sum)", cost)
				}
			},
		},
		{
			name:      "no model resolvable: tokens recorded, cost omitted",
			taskModel: "",
			effective: "",
			wantModel: "",
			wantCost: func(t *testing.T, cost any) {
				if cost != nil {
					t.Fatalf("_cost_usd = %v, want absent for an unpriceable model", cost)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Result{
				Output:         map[string]any{},
				EffectiveModel: tt.effective,
			}
			annotateCost(&result, Task{Model: tt.taskModel}, 1000, 500, tt.rms...)
			if got := result.Output["_tokens"]; got != 1500 {
				t.Errorf("_tokens = %v, want 1500", got)
			}
			if got := result.Output["_model"]; got != tt.wantModel {
				t.Errorf("_model = %v, want %q", got, tt.wantModel)
			}
			tt.wantCost(t, result.Output["_cost_usd"])
		})
	}
}
