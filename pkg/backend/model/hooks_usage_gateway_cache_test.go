package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/cost"
)

// Revi R7402a3: on a gateway model the wire's prompt count ALREADY includes
// the cached share (OpenAI convention — prompt_tokens_details.cached_tokens
// is a subset of prompt_tokens). The pacing estimator must not add
// CacheReadTokens on top of InputTokens nor price it with the Anthropic
// 0.1x multiplier: 1M input of which 800k cached + 100k output at
// 0.60/1.80/0.12 per M is 200k*0.60 + 800k*0.12 + 100k*1.80 = $0.396 and
// 1.1M tokens — not the Anthropic-accounting $0.828 and 1.9M tokens that
// let a cost_gt supervisor fire at less than half the real spend.
func TestUsageProgress_GatewayCacheReadCountsOnce(t *testing.T) {
	// Not parallel: swaps the process-wide gateway pricer.
	stub := func(string) (float64, float64, float64, bool) { return 0.60, 1.80, 0.12, true }
	cost.SetGatewayPricer(stub, stub)
	t.Cleanup(func() { cost.SetGatewayPricer(compatgw.StaticPricer, compatgw.StaticPricerQuiet) })

	hooks, load := newUsageHooks(t, "run-up-gw-cache")

	hooks.OnLLMRequest("review", LLMRequestInfo{Model: "openai_compatible/scaleway/deepseek-v4.1-flash"})
	hooks.OnLLMStepFinish("review", LLMStepInfo{
		Number: 1, InputTokens: 600_000, CacheReadTokens: 400_000, OutputTokens: 60_000,
	})
	hooks.OnLLMStepFinish("review", LLMStepInfo{
		Number: 2, InputTokens: 400_000, CacheReadTokens: 400_000, OutputTokens: 40_000,
	})

	evts := load()
	if len(evts) < 1 {
		t.Fatal("gateway step feed emitted nothing")
	}
	last := evts[len(evts)-1]
	used, _ := last.Data["used"].(float64)
	if want := 0.396; used != want {
		t.Errorf("pacing priced the cached share twice: used = %v, want %v (cache read is inside the gateway input count)", used, want)
	}
	tok, _ := last.Data["tokens"].(float64)
	if want := 1_100_000.0; tok != want {
		t.Errorf("pacing counted the cached share twice: tokens = %v, want %v", tok, want)
	}
}

// Anthropic-side claw steps keep their exclusive accounting: cache reads
// are NOT part of InputTokens there, they add to the token total and price
// at the 0.1x multiplier the pacing estimate has always used.
func TestUsageProgress_AnthropicCacheReadStaysExclusive(t *testing.T) {
	hooks, load := newUsageHooks(t, "run-up-anthropic-cache")

	hooks.OnLLMRequest("review", LLMRequestInfo{Model: "claude-opus-5"})
	hooks.OnLLMStepFinish("review", LLMStepInfo{
		Number: 1, InputTokens: 100_000, CacheReadTokens: 800_000, OutputTokens: 10_000,
	})

	evts := load()
	if len(evts) < 1 {
		t.Fatal("anthropic step feed emitted nothing")
	}
	last := evts[len(evts)-1]
	// Token total keeps the cache read on top: 100k + 800k + 10k = 910k.
	tok, _ := last.Data["tokens"].(float64)
	if want := 910_000.0; tok != want {
		t.Errorf("anthropic cache read must stay additive, tokens = %v, want %v", tok, want)
	}
}
