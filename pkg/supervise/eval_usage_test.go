package supervise

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// proseClient answers every request with billed prose and no tool call.
type proseClient struct {
	mu    sync.Mutex
	calls int
}

func (c *proseClient) StreamResponse(context.Context, api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 900},
		{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "I would rather explain in prose."}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{Reported: true, OutputTokens: 60}},
		{Type: api.EventMessageStop},
	}
	ch := make(chan api.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// An evaluation whose model never answers the decision schema fails — and
// still returns what its turns were billed (the structured call's forced
// turn and its one re-ask), which the coordinator books.
func TestLLMEvaluator_AFailedEvaluationReturnsWhatItSpent(t *testing.T) {
	client := &proseClient{}
	e := &LLMEvaluator{client: client, modelSpec: "anthropic/claude-sonnet-4-6"}
	_, usage, err := e.Evaluate(context.Background(), EvalInput{Spec: Spec{Name: "coach"}, ActiveNode: "n", WakeReason: "turn_boundary"})
	if err == nil {
		t.Fatal("an evaluation with no decision succeeded")
	}
	if client.calls != 2 {
		t.Fatalf("%d calls, want the forced turn and its re-ask", client.calls)
	}
	if usage.InputTokens != 1800 || usage.OutputTokens != 120 {
		t.Errorf("usage = %+v, want both billed turns (1800 in / 120 out)", usage)
	}
}
