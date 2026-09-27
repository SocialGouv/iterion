package model

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// captureRequestClient is an api.APIClient that records every request so a
// test can assert on the exact payload a model would receive.
type captureRequestClient struct {
	mu     sync.Mutex
	reqs   []api.CreateMessageRequest
	stream <-chan api.StreamEvent
}

func (c *captureRequestClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
	return c.stream, nil
}

func (c *captureRequestClient) last() api.CreateMessageRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqs[len(c.reqs)-1]
}

// TestClawBackend_EffortNoneReachesTheWire is the simulated-payload half of
// issue #1837: an authored `reasoning_effort: none` must reach the provider
// request verbatim on the models that carry it (GPT-6 Sol/Luna), and clamp to
// the lowest real level everywhere else — never be erased or re-leveled
// upward. Anthropic's Opus 5.5 has no none level, so it floors at low, the
// same coercion the claude_code delegate applies on the CLI route.
func TestClawBackend_EffortNoneReachesTheWire(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		effort string
		want   string
	}{
		{name: "sol keeps none", model: "openai/gpt-6-sol", effort: "none", want: "none"},
		{name: "luna keeps none", model: "openai/gpt-6-luna", effort: "none", want: "none"},
		{name: "astra floors none to low", model: "openai/gpt-6-astra", effort: "none", want: "low"},
		{name: "opus 5.5 floors none to low", model: "anthropic/claude-opus-5-5", effort: "none", want: "low"},
		{name: "other levels pass through", model: "openai/gpt-6-sol", effort: "high", want: "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			mock := &captureRequestClient{stream: mockStreamEvents("ok", "end_turn")}
			reg.Register("openai", func(modelID string) (api.APIClient, error) {
				return mock, nil
			})
			reg.Register("anthropic", func(modelID string) (api.APIClient, error) {
				return mock, nil
			})

			backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{})
			_, err := backend.Execute(context.Background(), delegate.Task{
				NodeID:          "agent1",
				Model:           tc.model,
				UserPrompt:      "ping",
				ReasoningEffort: tc.effort,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := mock.last().ReasoningEffort; got != tc.want {
				t.Errorf("wire reasoning_effort = %q, want %q", got, tc.want)
			}
		})
	}
}
