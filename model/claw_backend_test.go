package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"claw-code-go/pkg/api"

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
