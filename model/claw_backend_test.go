package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/delegate"
	"github.com/SocialGouv/iterion/ir"
)

// ---------------------------------------------------------------------------
// ClawBackend unit tests
// ---------------------------------------------------------------------------

// TestClawBackend_StructuredOutput verifies the structured output generation
// strategy (schema + no tools) produces correct delegate.Result output.
func TestClawBackend_StructuredOutput(t *testing.T) {
	reg := NewRegistry()
	ch := make(chan api.StreamEvent, 10)
	go func() {
		defer close(ch)
		ch <- api.StreamEvent{Type: api.EventMessageStart, InputTokens: 50}
		ch <- api.StreamEvent{
			Type:  api.EventContentBlockStart,
			Index: 0,
			ContentBlock: api.ContentBlockInfo{
				Type: "tool_use", Index: 0, ID: "tu_1", Name: "structured_output",
			},
		}
		ch <- api.StreamEvent{
			Type:  api.EventContentBlockDelta,
			Index: 0,
			Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"verdict":true,"reason":"Good"}`},
		}
		ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
		ch <- api.StreamEvent{
			Type:       api.EventMessageDelta,
			StopReason: "tool_use",
			Usage:      api.UsageDelta{OutputTokens: 25},
		}
		ch <- api.StreamEvent{Type: api.EventMessageStop}
	}()

	mock := &execMockClient{streams: []<-chan api.StreamEvent{ch}}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	schema := &ir.Schema{
		Name: "verdict",
		Fields: []*ir.SchemaField{
			{Name: "verdict", Type: ir.FieldTypeBool},
			{Name: "reason", Type: ir.FieldTypeString},
		},
	}
	schemaJSON, _ := SchemaToJSON(schema)

	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{})

	result, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "judge1",
		Model:        "test/test-model",
		SystemPrompt: "You are a judge.",
		UserPrompt:   "Review this code.",
		OutputSchema: schemaJSON,
		HasTools:     false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.BackendName != delegate.BackendClaw {
		t.Errorf("BackendName = %q, want %q", result.BackendName, delegate.BackendClaw)
	}
	if result.Tokens != 75 {
		t.Errorf("Tokens = %d, want 75", result.Tokens)
	}
	if result.Output["verdict"] != true {
		t.Errorf("verdict = %v, want true", result.Output["verdict"])
	}
	if result.Output["reason"] != "Good" {
		t.Errorf("reason = %v, want %q", result.Output["reason"], "Good")
	}
}

// TestClawBackend_TextGeneration verifies the plain text generation strategy.
func TestClawBackend_TextGeneration(t *testing.T) {
	reg := NewRegistry()
	mock := &execMockClient{
		streams: []<-chan api.StreamEvent{mockStreamEvents("Hello world", "end_turn")},
	}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{})

	result, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:     "agent1",
		Model:      "test/test-model",
		UserPrompt: "Say hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Output["text"] != "Hello world" {
		t.Errorf("text = %q, want %q", result.Output["text"], "Hello world")
	}
	if result.Tokens != 150 {
		t.Errorf("Tokens = %d, want 150", result.Tokens)
	}
	if result.BackendName != delegate.BackendClaw {
		t.Errorf("BackendName = %q, want %q", result.BackendName, delegate.BackendClaw)
	}
}

// TestClawBackend_TextWithToolsAndSchema verifies the text+tools+schema strategy.
func TestClawBackend_TextWithToolsAndSchema(t *testing.T) {
	reg := NewRegistry()
	mock := &execMockClient{
		streams: []<-chan api.StreamEvent{mockStreamEvents(`{"result":"computed","value":42}`, "end_turn")},
	}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	schema := &ir.Schema{
		Name: "compute",
		Fields: []*ir.SchemaField{
			{Name: "result", Type: ir.FieldTypeString},
			{Name: "value", Type: ir.FieldTypeInt},
		},
	}
	schemaJSON, _ := SchemaToJSON(schema)

	toolDefs := []delegate.ToolDef{
		{
			Name:        "calculator",
			Description: "A calculator",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
				return `{"answer":42}`, nil
			},
		},
	}

	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{})

	result, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "agent_with_tools",
		Model:        "test/test-model",
		UserPrompt:   "Compute the answer",
		OutputSchema: schemaJSON,
		HasTools:     true,
		ToolDefs:     toolDefs,
		ToolMaxSteps: 3,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Output["result"] != "computed" {
		t.Errorf("result = %v, want %q", result.Output["result"], "computed")
	}
	if result.BackendName != delegate.BackendClaw {
		t.Errorf("BackendName = %q, want %q", result.BackendName, delegate.BackendClaw)
	}
	if result.ParseFallback {
		t.Error("expected ParseFallback=false for valid JSON output")
	}
}

// TestClawBackend_RetryClassification verifies retry classification.
func TestClawBackend_RetryClassification(t *testing.T) {
	t.Run("retryable_APIError", func(t *testing.T) {
		reg := NewRegistry()
		mock := &failThenSucceedClient{
			streams:  []<-chan api.StreamEvent{mockStreamEvents("recovered", "end_turn")},
			failures: 1,
		}
		reg.Register("test", func(modelID string) (api.APIClient, error) {
			return mock, nil
		})

		var retries int
		hooks := EventHooks{
			OnLLMRetry: func(_ string, _ RetryInfo) { retries++ },
		}
		backend := NewClawBackend(reg, hooks, RetryPolicy{
			MaxAttempts: 3,
			BackoffBase: time.Millisecond,
		})

		result, err := backend.Execute(context.Background(), delegate.Task{
			NodeID:     "agent1",
			Model:      "test/test-model",
			UserPrompt: "hello",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if retries != 1 {
			t.Errorf("retries = %d, want 1", retries)
		}
		if result.Output["text"] != "recovered" {
			t.Errorf("text = %q, want %q", result.Output["text"], "recovered")
		}
	})

	t.Run("retryable_clawAPIError_429", func(t *testing.T) {
		// Phase 2.2 coverage: an *api.APIError (claw-code-go's public typed
		// error, returned by the OpenAI/Anthropic provider HTTP layer on
		// non-2xx responses) must drive iterion's retry loop the same way
		// iterion's local *APIError does. Without this, transient HTTP
		// errors from claw providers leak to the user and never retry.
		reg := NewRegistry()
		mock := &clawErrThenSucceedClient{
			streams: []<-chan api.StreamEvent{mockStreamEvents("recovered after 429", "end_turn")},
			err: &api.APIError{
				Provider:   "openai",
				StatusCode: 429,
				Message:    "rate limited (mock)",
				Retryable:  true,
			},
			failures: 1,
		}
		reg.Register("test", func(modelID string) (api.APIClient, error) {
			return mock, nil
		})

		var retries int
		var lastStatus int
		hooks := EventHooks{
			OnLLMRetry: func(_ string, info RetryInfo) {
				retries++
				lastStatus = info.StatusCode
			},
		}
		backend := NewClawBackend(reg, hooks, RetryPolicy{
			MaxAttempts: 3,
			BackoffBase: time.Millisecond,
		})

		result, err := backend.Execute(context.Background(), delegate.Task{
			NodeID:     "agent1",
			Model:      "test/test-model",
			UserPrompt: "hello",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if retries != 1 {
			t.Errorf("retries = %d, want 1", retries)
		}
		if lastStatus != 429 {
			t.Errorf("RetryInfo.StatusCode = %d, want 429", lastStatus)
		}
		if got, _ := result.Output["text"].(string); got != "recovered after 429" {
			t.Errorf("text = %q, want %q", got, "recovered after 429")
		}
	})

	t.Run("non_retryable_clawAPIError_403", func(t *testing.T) {
		// 403 is NOT in {408, 409, 429, 5xx}; retry must not fire.
		reg := NewRegistry()
		mock := &execMockClient{
			err: &api.APIError{
				Provider:   "openai",
				StatusCode: 403,
				Message:    "forbidden",
				Retryable:  false,
			},
		}
		reg.Register("test", func(modelID string) (api.APIClient, error) {
			return mock, nil
		})

		var retries int
		hooks := EventHooks{
			OnLLMRetry: func(_ string, _ RetryInfo) { retries++ },
		}
		backend := NewClawBackend(reg, hooks, RetryPolicy{
			MaxAttempts: 3,
			BackoffBase: time.Millisecond,
		})

		_, err := backend.Execute(context.Background(), delegate.Task{
			NodeID:     "agent1",
			Model:      "test/test-model",
			UserPrompt: "hello",
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if retries != 0 {
			t.Errorf("retries = %d, want 0 (403 is not retryable)", retries)
		}
	})

	t.Run("non_retryable_error", func(t *testing.T) {
		reg := NewRegistry()
		mock := &execMockClient{
			err: &APIError{Message: "forbidden", StatusCode: 403, IsRetryable: false},
		}
		reg.Register("test", func(modelID string) (api.APIClient, error) {
			return mock, nil
		})

		var retries int
		hooks := EventHooks{
			OnLLMRetry: func(_ string, _ RetryInfo) { retries++ },
		}
		backend := NewClawBackend(reg, hooks, RetryPolicy{
			MaxAttempts: 3,
			BackoffBase: time.Millisecond,
		})

		_, err := backend.Execute(context.Background(), delegate.Task{
			NodeID:     "agent1",
			Model:      "test/test-model",
			UserPrompt: "hello",
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if retries != 0 {
			t.Errorf("retries = %d, want 0 (non-retryable errors should not retry)", retries)
		}
	})
}

// TestClawBackend_HookEmissionOrdering verifies hook emission order.
func TestClawBackend_HookEmissionOrdering(t *testing.T) {
	reg := NewRegistry()
	mock := &execMockClient{
		streams: []<-chan api.StreamEvent{mockStreamEvents("result", "end_turn")},
	}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	var order []string
	hooks := EventHooks{
		OnLLMRequest: func(nodeID string, info LLMRequestInfo) {
			order = append(order, "request:"+nodeID)
		},
		OnLLMResponse: func(nodeID string, info LLMResponseInfo) {
			order = append(order, "response:"+nodeID)
		},
	}

	backend := NewClawBackend(reg, hooks, RetryPolicy{})

	_, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:     "agent1",
		Model:      "test/test-model",
		UserPrompt: "hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(order) < 2 {
		t.Fatalf("expected at least 2 hook calls, got %d: %v", len(order), order)
	}
	if order[0] != "request:agent1" {
		t.Errorf("order[0] = %q, want %q", order[0], "request:agent1")
	}
	if order[1] != "response:agent1" {
		t.Errorf("order[1] = %q, want %q", order[1], "response:agent1")
	}
}

// requestCapturingClient records every CreateMessageRequest passed to
// StreamResponse, then returns a one-shot deterministic stream.
type requestCapturingClient struct {
	requests []api.CreateMessageRequest
}

func (c *requestCapturingClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.requests = append(c.requests, req)
	ch := make(chan api.StreamEvent, 6)
	ch <- api.StreamEvent{Type: api.EventMessageStart, InputTokens: 10}
	ch <- api.StreamEvent{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}}
	ch <- api.StreamEvent{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "ok"}}
	ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
	ch <- api.StreamEvent{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 5}}
	ch <- api.StreamEvent{Type: api.EventMessageStop}
	close(ch)
	return ch, nil
}

// newCapturingBackend wires a registry, a requestCapturingClient under provider
// "test", and a fresh ClawBackend. Used by the cache_control / max_tokens tests
// that need to assert the exact wire-level CreateMessageRequest.
func newCapturingBackend() (*ClawBackend, *requestCapturingClient) {
	reg := NewRegistry()
	cap := &requestCapturingClient{}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
		return cap, nil
	})
	return NewClawBackend(reg, EventHooks{}, RetryPolicy{}), cap
}

func TestClawBackend_SystemPromptUsesEphemeralCacheControl(t *testing.T) {
	backend, cap := newCapturingBackend()

	_, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "agent1",
		Model:        "test/test-model",
		SystemPrompt: "You are a helpful assistant.",
		UserPrompt:   "Say hello",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(cap.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(cap.requests))
	}
	req := cap.requests[0]
	if req.System != "" {
		t.Errorf("req.System = %q, want empty (should use SystemBlocks)", req.System)
	}
	if len(req.SystemBlocks) != 1 {
		t.Fatalf("SystemBlocks len = %d, want 1", len(req.SystemBlocks))
	}
	block := req.SystemBlocks[0]
	if block.Type != "text" {
		t.Errorf("block.Type = %q, want text", block.Type)
	}
	if block.Text != "You are a helpful assistant." {
		t.Errorf("block.Text = %q", block.Text)
	}
	if block.CacheControl == nil {
		t.Error("expected ephemeral cache_control marker on system block")
	} else if block.CacheControl.Type != "ephemeral" {
		t.Errorf("CacheControl.Type = %q, want ephemeral", block.CacheControl.Type)
	}
}

func TestClawBackend_NoSystemPromptDoesNotPopulateBlocks(t *testing.T) {
	backend, cap := newCapturingBackend()

	_, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:     "agent1",
		Model:      "test/test-model",
		UserPrompt: "Say hello",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(cap.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(cap.requests))
	}
	if len(cap.requests[0].SystemBlocks) != 0 {
		t.Errorf("SystemBlocks should be empty, got %d entries", len(cap.requests[0].SystemBlocks))
	}
	if cap.requests[0].System != "" {
		t.Errorf("System should be empty, got %q", cap.requests[0].System)
	}
}

func TestClawBackend_TaskMaxTokensPropagated(t *testing.T) {
	backend, cap := newCapturingBackend()

	_, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:     "agent1",
		Model:      "test/test-model",
		UserPrompt: "go",
		MaxTokens:  256,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(cap.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(cap.requests))
	}
	if cap.requests[0].MaxTokens != 256 {
		t.Errorf("req.MaxTokens = %d, want 256", cap.requests[0].MaxTokens)
	}
}

func TestClawBackend_TaskMaxTokensZeroFallsBackToDefault(t *testing.T) {
	backend, cap := newCapturingBackend()

	_, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:     "agent1",
		Model:      "test/test-model",
		UserPrompt: "go",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if cap.requests[0].MaxTokens != defaultMaxTokens {
		t.Errorf("req.MaxTokens = %d, want %d (default)", cap.requests[0].MaxTokens, defaultMaxTokens)
	}
}
