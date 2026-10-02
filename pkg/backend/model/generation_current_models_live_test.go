//go:build live

package model

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/cost"
)

// Opt in with a comma-separated list of provider/model specs. This uses the
// normal registry credentials, sends only synthetic data and performs no writes.
// Run with -tags live -run TestLiveCurrentModels -count=1 -v.
func TestLiveCurrentModels(t *testing.T) {
	specs := os.Getenv("ITERION_LIVE_CURRENT_MODELS")
	if specs == "" {
		t.Skip("set ITERION_LIVE_CURRENT_MODELS to opt into paid compatibility probes")
	}
	for _, spec := range strings.Split(specs, ",") {
		t.Run(spec, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			client, err := NewRegistry().ResolveWithContext(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			called := 0
			out, err := GenerateTextDirect(ctx, client, GenerationOptions{
				Model: spec, ProviderOptions: map[string]any{"reasoning_effort": "low"}, MaxTokens: 2048, MaxSteps: 3, ForceInitialToolUse: true,
				Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Call read_probe once, then reply with the observed value, without any other text."}}}},
				Tools:    []GenerationTool{{Name: "read_probe", Description: "Read the compatibility probe value", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`), Execute: func(context.Context, json.RawMessage) (string, error) { called++; return "migration-ok", nil }}},
			})
			if out != nil {
				t.Logf("model=%s tool_loop input=%d output=%d estimate_usd=%.6f", spec, out.TotalUsage.InputTokens, out.TotalUsage.OutputTokens, cost.EstimateUSD(spec, out.TotalUsage.InputTokens, out.TotalUsage.OutputTokens))
			}
			if err != nil {
				t.Fatal(err)
			}
			if called != 1 || !strings.Contains(out.Text, "migration-ok") {
				t.Fatalf("tool loop: calls=%d text=%q", called, out.Text)
			}
			obj, err := GenerateObjectDirect[map[string]any](ctx, client, GenerationOptions{
				Model: spec, ProviderOptions: map[string]any{"reasoning_effort": "low"}, MaxTokens: 2048, SchemaName: "probe_result",
				ExplicitSchema: json.RawMessage(`{"type":"object","properties":{"payload":{"type":"object","additionalProperties":true}},"required":["payload"]}`),
				Messages:       []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: `Return payload with a dynamic key "unlisted" containing {"nested": [true, 7]}.`}}}},
			})
			if obj != nil {
				t.Logf("model=%s structured input=%d output=%d estimate_usd=%.6f", spec, obj.TotalUsage.InputTokens, obj.TotalUsage.OutputTokens, cost.EstimateUSD(spec, obj.TotalUsage.InputTokens, obj.TotalUsage.OutputTokens))
			}
			if err != nil {
				t.Fatal(err)
			}
			payload, ok := obj.Object["payload"].(map[string]any)
			if !ok || payload["unlisted"] == nil {
				t.Fatalf("dynamic JSON missing: %+v", obj.Object)
			}
		})
	}
}
