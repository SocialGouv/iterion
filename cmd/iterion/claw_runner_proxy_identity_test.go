package main

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// scriptedClient answers each request with the next scripted stream.
type scriptedClient struct{ streams [][]api.StreamEvent }

func (c *scriptedClient) StreamResponse(context.Context, api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	next := c.streams[0]
	c.streams = c.streams[1:]
	ch := make(chan api.StreamEvent, len(next))
	for _, e := range next {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// TestProxyCarriesTheCallIdentityToTheLauncher: a call the runner forwards
// carries the tool_use id the model gave it, read from the context the door
// hands the tool, so a refusal the launcher records names the same call as
// the runner's own event. Driven through the real generation loop, the real
// proxy and the real multiplexer.
func TestProxyCarriesTheCallIdentityToTheLauncher(t *testing.T) {
	runnerStdinR, runnerStdinW := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()
	dispatcher := newProxyDispatcher(runnerStdinR, runnerStdoutW)
	dispatcher.start()
	proxies := launcherProxies(t, dispatcher, delegate.IOToolDef{Name: "list_mcp_resources"})

	seen := make(chan [2]string, 1)
	mux := delegate.NewMultiplexer(runnerStdoutR, runnerStdinW, delegate.MultiplexerHandler{
		OnToolCall: func(ctx context.Context, _ string, _ json.RawMessage) (string, error) {
			id, parent := delegate.ForwardedToolUseFrom(ctx)
			seen <- [2]string{id, parent}
			return "no resources", nil
		},
	})
	go func() { _, _ = mux.Run(context.Background()) }()

	client := &scriptedClient{streams: [][]api.StreamEvent{
		{
			{Type: api.EventMessageStart, InputTokens: 10},
			{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "toolu_fwd", Name: "list_mcp_resources"}},
			{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{}`}},
			{Type: api.EventContentBlockStop, Index: 0},
			{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{Reported: true, OutputTokens: 5}},
			{Type: api.EventMessageStop},
		},
		{
			{Type: api.EventMessageStart, InputTokens: 10},
			{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
			{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "done"}},
			{Type: api.EventContentBlockStop, Index: 0},
			{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{Reported: true, OutputTokens: 5}},
			{Type: api.EventMessageStop},
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := model.GenerateTextDirect(ctx, client, model.GenerationOptions{
		Model:    "test-model",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}}}},
		Tools: []model.GenerationTool{{
			Name: proxies[0].Name, Description: "list", InputSchema: json.RawMessage(`{"type":"object"}`),
			Execute: proxies[0].Execute,
		}},
	}); err != nil {
		t.Fatalf("GenerateTextDirect: %v", err)
	}
	select {
	case got := <-seen:
		if got != [2]string{"toolu_fwd", ""} {
			t.Errorf("the launcher saw the forwarded call as id=%q parent=%q, want the model's toolu_fwd and no parent", got[0], got[1])
		}
	default:
		t.Fatal("the forwarded call never reached the launcher")
	}
}
