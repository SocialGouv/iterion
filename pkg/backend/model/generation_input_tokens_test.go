package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

func drainAggregate(t *testing.T, events []api.StreamEvent) aggregatedResponse {
	t.Helper()
	ch := make(chan api.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	agg := aggregateStream(context.Background(), ch)
	if agg.err != nil {
		t.Fatalf("aggregate: %v", agg.err)
	}
	return agg
}

// The OpenAI endpoints have no message_start-shaped frame to carry a
// prompt count on: both learn it only in the terminal usage payload, so
// claw reports it on message_delta. Reading output there and not input
// priced every OpenAI turn as if it had cost nothing to send — which
// matters beyond analysis, since a metered key's spend is computed
// input×rate + output×rate and feeds an org's monthly cap.
func TestAggregateStream_TakesInputTokensFromTheDeltaWhenThatIsWhereTheyAre(t *testing.T) {
	agg := drainAggregate(t, []api.StreamEvent{
		{Type: api.EventMessageStart},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "hi"}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{InputTokens: 4321, OutputTokens: 9}},
		{Type: api.EventMessageStop},
	})
	if agg.usage.InputTokens != 4321 {
		t.Errorf("InputTokens = %d, want 4321 — an OpenAI turn reads as free to send", agg.usage.InputTokens)
	}
	if agg.usage.OutputTokens != 9 {
		t.Errorf("OutputTokens = %d, want 9", agg.usage.OutputTokens)
	}
}

// The other direction, and the one a careless fix breaks: Anthropic and
// bedrock answer on message_start and send a delta whose input count is
// zero. Taking the delta unconditionally would erase the real number.
func TestAggregateStream_DeltaZeroDoesNotEraseTheMessageStartCount(t *testing.T) {
	agg := drainAggregate(t, []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 100, CacheReadInputTokens: 7},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "hi"}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 20}},
		{Type: api.EventMessageStop},
	})
	if agg.usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100 — a delta carrying nothing overwrote the message_start count", agg.usage.InputTokens)
	}
	if agg.usage.CacheReadTokens != 7 {
		t.Errorf("CacheReadTokens = %d, want 7", agg.usage.CacheReadTokens)
	}
}
