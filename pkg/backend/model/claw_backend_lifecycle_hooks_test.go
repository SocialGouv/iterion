package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// requestCaptureClient records every CreateMessageRequest the loop sends,
// so a test can read the tool_result the model actually receives.
type requestCaptureClient struct {
	execMockClient
	requests []api.CreateMessageRequest
}

func (c *requestCaptureClient) StreamResponse(ctx context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.requests = append(c.requests, req)
	return c.execMockClient.StreamResponse(ctx, req)
}

// streamOf feeds a slice of events as a completed stream channel.
func streamOf(events []api.StreamEvent) <-chan api.StreamEvent {
	ch := make(chan api.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch
}

// TestClawBackend_DefaultLifecycleHookBlocksInsideTheLoop is the wiring
// witness behind integrations.claw-lifecycle-hooks: iterion DOES install a
// lifecycle runner on its claw client (runview hands NewDefaultLifecycleHooks
// through WithBackendLifecycleHooks), and that runner's PreToolUse decision
// reaches the real conversation loop — a `rm -rf /` call is refused before
// the tool's Execute runs, and the refusal travels to the model as the
// tool_result it can read and adapt to.
//
// Mutation: delete `opts.Hooks = b.lifecycleHooks` from ClawBackend.Execute —
// bash runs and the `ran` guard reddens; the refusal assertions redden with
// it. The hook's own patterns are pinned separately by
// TestNewDefaultLifecycleHooks_BlocksDangerousCommand; this test carries the
// WIRING, not the regexes.
func TestClawBackend_DefaultLifecycleHookBlocksInsideTheLoop(t *testing.T) {
	t.Parallel()

	mock := &requestCaptureClient{
		execMockClient: execMockClient{
			streams: []<-chan api.StreamEvent{
				streamOf(toolUseEvents("tu_1", "bash", `{"command":"rm -rf /"}`, 100, 30)),
				streamOf(textEvents("done", 150, 25)),
			},
		},
	}
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })

	ran := false
	bashTool := delegate.ToolDef{
		Name:        "bash",
		Description: "run a shell command",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			ran = true
			return "destroyed", nil
		},
	}

	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{},
		WithBackendLifecycleHooks(NewDefaultLifecycleHooks(EventHooks{})))

	_, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "hooked",
		Model:        "test/test-model",
		UserPrompt:   "clean the disk",
		HasTools:     true,
		ToolDefs:     []delegate.ToolDef{bashTool},
		ToolMaxSteps: 3,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ran {
		t.Fatal("the default lifecycle hook must refuse `rm -rf /` before Execute — the tool ran anyway")
	}
	if mock.calls != 2 {
		t.Fatalf("model calls = %d, want 2 (tool turn, then the turn over the refusal)", mock.calls)
	}

	// The refusal the model received: an isError tool_result for tu_1 whose
	// text names the safety hook, so the model can adapt instead of retrying
	// blind. Mutation: drop the Reason from preToolUseRefusal and this reddens.
	var refusal string
	for _, msg := range mock.requests[1].Messages {
		for _, block := range msg.Content {
			if block.Type != "tool_result" || block.ToolUseID != "tu_1" {
				continue
			}
			for _, inner := range block.Content {
				if inner.Type == "text" {
					refusal = inner.Text
				}
			}
		}
	}
	if !strings.Contains(refusal, "tool refused") || !strings.Contains(refusal, "safety hook blocked dangerous command") {
		t.Errorf("the model received %q, want the safety hook's refusal for tu_1", refusal)
	}
}
