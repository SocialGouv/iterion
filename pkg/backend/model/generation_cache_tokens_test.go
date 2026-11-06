package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// The chat-completions hosts (LiteLLM serving deepseek among them) report
// the prompt cache they served as prompt_tokens_details.cached_tokens,
// riding the same terminal usage payload as the prompt count — so claw
// reports it on message_delta. A metered run serves most of its prompt
// from cache; dropping the count prices every cached token at the full
// input rate and the run's cost over-reads against the gateway's own bill.
func TestAggregateStream_TakesCacheReadTokensFromTheDelta(t *testing.T) {
	t.Parallel()
	delta := api.UsageDelta{Reported: true, InputTokens: 4321, OutputTokens: 9}
	delta.InputTokensDetails.CachedTokens = 4000
	agg := drainAggregate(t, []api.StreamEvent{
		{Type: api.EventMessageStart},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "hi"}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: delta},
		{Type: api.EventMessageStop},
	})
	if agg.usage.CacheReadTokens != 4000 {
		t.Errorf("CacheReadTokens = %d, want 4000 — the cache read fell off the meter", agg.usage.CacheReadTokens)
	}
	if agg.usage.InputTokens != 4321 {
		t.Errorf("InputTokens = %d, want 4321", agg.usage.InputTokens)
	}
}

// The Anthropic direction: message_start carries the cache read and the
// delta's cache field is its zero value. An unconditional read would erase
// the real number — the guard takes the delta only when non-zero.
func TestAggregateStream_DeltaZeroCacheDoesNotEraseTheMessageStartCache(t *testing.T) {
	t.Parallel()
	agg := drainAggregate(t, []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 100, CacheReadInputTokens: 7},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "hi"}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{Reported: true, OutputTokens: 20}},
		{Type: api.EventMessageStop},
	})
	if agg.usage.CacheReadTokens != 7 {
		t.Errorf("CacheReadTokens = %d, want 7 — a delta carrying nothing overwrote the message_start cache", agg.usage.CacheReadTokens)
	}
}

// A stream that fails after the provider served a cached prompt still
// spent that cache: the error frame carries what the stream had reported,
// and the cache read merges by max like the other counters.
func TestAggregateStream_ErrorMergesTheCacheReadItCarries(t *testing.T) {
	t.Parallel()
	errUsage := api.UsageDelta{Reported: true, InputTokens: 500, OutputTokens: 3}
	errUsage.InputTokensDetails.CachedTokens = 450
	ch := make(chan api.StreamEvent, 8)
	for _, ev := range []api.StreamEvent{
		{Type: api.EventMessageStart},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "hi"}},
		{Type: api.EventError, ErrorMessage: "stream reset", Usage: errUsage},
	} {
		ch <- ev
	}
	close(ch)
	agg := aggregateStream(context.Background(), ch)
	if agg.err == nil {
		t.Fatal("expected the stream error to surface")
	}
	if agg.usage.CacheReadTokens != 450 {
		t.Errorf("CacheReadTokens = %d, want 450 — the error frame's cache account was dropped", agg.usage.CacheReadTokens)
	}
}
