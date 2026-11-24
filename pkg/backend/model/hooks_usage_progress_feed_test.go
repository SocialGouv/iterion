package model

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// W12 — the usage_progress sample of a gateway node prices through the
// run's context: the run table priced glm-5.3, the runner's process env
// carries no table, and the cost_gt monitor still reads a USD figure.
func TestUsageProgress_GatewaySamplePricesThroughTheRunContext(t *testing.T) {
	unsetGatewayTables(t)
	table := `{"entries":{"glm-5.3":{"feed":"pliage","refreshed_at":"` + time.Now().UTC().Format(time.RFC3339) + `","fresh_for":0,"declared":true,"spec":{"context_window":256000,"input_cost_per_m":2.01,"output_cost_per_m":6.15}}}}`
	ctx := compatgw.OverlayFeedTable(context.Background(), []byte(table))
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(context.Background(), "run-up-feed", "wf", nil); err != nil {
		t.Fatal(err)
	}
	hooks := NewStoreEventHooks(ctx, st, "run-up-feed", iterlog.Nop(), nil, nil)
	hooks.OnLLMRequest("review", LLMRequestInfo{Model: modelroute.OpenAICompatible + "/glm-5.3"})
	hooks.OnLLMStepFinish("review", LLMStepInfo{Number: 1, InputTokens: 100000, OutputTokens: 5000})
	all, err := st.LoadEvents(context.Background(), "run-up-feed")
	if err != nil {
		t.Fatal(err)
	}
	var samples []store.Event
	for _, e := range all {
		if e.Type == store.EventUsageProgress {
			samples = append(samples, *e)
		}
	}
	if len(samples) != 1 {
		t.Fatalf("usage_progress samples = %d, want 1", len(samples))
	}
	// 100k input at 2.01 + 5k output at 6.15 per Mtok = 0.23175 (the
	// sample rounds to four places).
	if used, _ := samples[0].Data["used"].(float64); used < 0.2317 || used > 0.2318 {
		t.Fatalf("the sample's used = %v, want 0.23175 rounded — priced from the run table", samples[0].Data["used"])
	}
}
