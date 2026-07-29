package model

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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

// TestClawBackend_ZeroToolCallNudgeReRunsLoop verifies the FINDING-3
// guard: a tool-equipped node that ends the agentic loop having made NO
// tool calls and produced non-JSON narration (the "review is still in
// progress" failure mode) gets ONE nudge re-run before the structured
// recovery. The first stream is pure narration; the second (the nudge
// re-run) is the real JSON verdict. Without the guard the narration would
// fall straight into the GenerateObjectDirect recovery, which forces a
// synthetic-tool tool_use — the plain-text 2nd stream would not satisfy
// it and Execute would error. So a clean parse here proves the re-run ran.
func TestClawBackend_ZeroToolCallNudgeReRunsLoop(t *testing.T) {
	reg := NewRegistry()
	mock := &execMockClient{streams: []<-chan api.StreamEvent{
		mockStreamEvents("I will start by reviewing the branch diff.", "end_turn"),
		mockStreamEvents(`{"approved":true,"summary":"looks good"}`, "end_turn"),
	}}
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })

	schema := &ir.Schema{
		Name: "verdict",
		Fields: []*ir.SchemaField{
			{Name: "approved", Type: ir.FieldTypeBool},
			{Name: "summary", Type: ir.FieldTypeString},
		},
	}
	schemaJSON, _ := SchemaToJSON(schema)

	toolDefs := []delegate.ToolDef{{
		Name:        "bash",
		Description: "Run a shell command",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { return "diff output", nil },
	}}

	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{})
	result, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "reviewer_gpt",
		Model:        "test/test-model",
		UserPrompt:   "Review the branch.",
		OutputSchema: schemaJSON,
		HasTools:     true,
		ToolDefs:     toolDefs,
		ToolMaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Output["approved"] != true {
		t.Errorf("approved = %v, want true (the nudge re-run should surface the real verdict, not a provisional one)", result.Output["approved"])
	}
	if mock.calls != 2 {
		t.Errorf("StreamResponse calls = %d, want 2 (initial narration + one nudge re-run); a different count means the guard mis-fired or recovery ran", mock.calls)
	}
}

// TestClawBackend_NudgeSkippedWhenModelStructuresDirectly verifies the
// guard is a no-op when the model already committed to a JSON verdict on
// its first turn even with zero tool calls — the legitimate inline-context
// reviewer (e.g. whole_improve_loop's chunk_content). It must NOT pay for
// an extra re-run.
func TestClawBackend_NudgeSkippedWhenModelStructuresDirectly(t *testing.T) {
	reg := NewRegistry()
	mock := &execMockClient{streams: []<-chan api.StreamEvent{
		mockStreamEvents(`{"approved":true,"summary":"inline review"}`, "end_turn"),
	}}
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })

	schema := &ir.Schema{
		Name: "verdict",
		Fields: []*ir.SchemaField{
			{Name: "approved", Type: ir.FieldTypeBool},
			{Name: "summary", Type: ir.FieldTypeString},
		},
	}
	schemaJSON, _ := SchemaToJSON(schema)
	toolDefs := []delegate.ToolDef{{
		Name:        "bash",
		Description: "Run a shell command",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { return "", nil },
	}}

	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{})
	result, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "reviewer_inline",
		Model:        "test/test-model",
		UserPrompt:   "Review the inline content.",
		OutputSchema: schemaJSON,
		HasTools:     true,
		ToolDefs:     toolDefs,
		ToolMaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Output["approved"] != true {
		t.Errorf("approved = %v, want true", result.Output["approved"])
	}
	if mock.calls != 1 {
		t.Errorf("StreamResponse calls = %d, want 1 (direct JSON verdict must NOT trigger the tool-use nudge)", mock.calls)
	}
}

func TestLooksStructured(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{`{"approved":true}`, true},
		{"  \n{\"a\":1}\n", true},
		{"Here is my verdict:\n{\"approved\":false}", true}, // extractJSON pulls the object out
		{"I will start by reviewing the branch diff.", false},
		{"", false},
		{"Review is still in progress; no final verdict yet.", false},
	}
	for _, c := range cases {
		if got := looksStructured(c.text); got != c.want {
			t.Errorf("looksStructured(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// TestClassifyStreamEventError verifies that transport / truncation stream
// error events are classified RETRYABLE while permanent provider errors
// stay terminal — the detection half of the network-resilience hardening.
func TestClassifyStreamEventError(t *testing.T) {
	cases := []struct {
		name      string
		msg       string
		retryable bool
	}{
		{"anthropic read-stream drop", "read stream: connection reset by peer", true},
		{"openai stream read error", "openai stream read: unexpected EOF", true},
		{"openai clean-eof truncation", "openai stream truncated: closed without finish_reason or [DONE]", true},
		{"bare i/o timeout", "i/o timeout", true},
		{"upstream overloaded", "upstream overloaded", true},
		{"sse parse mid-drop", "parse SSE: unexpected end of JSON input", true},
		{"permanent context overflow", `{"type":"error","error":{"code":"context_length_exceeded"}}`, false},
		{"permanent content refusal", "model refused: content policy violation", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := classifyStreamEventError(c.msg)
			if err == nil {
				t.Fatal("expected a non-nil error")
			}
			if got := isRetryable(err); got != c.retryable {
				t.Errorf("isRetryable(classifyStreamEventError(%q)) = %v, want %v", c.msg, got, c.retryable)
			}
		})
	}
}

// streamThenErrorClient returns a scripted stream on the first call, then
// a fixed error on every subsequent call — used to fail a SPECIFIC later
// call (e.g. the nudge re-run) while the initial turn succeeds.
type streamThenErrorClient struct {
	first <-chan api.StreamEvent
	err   error
	mu    sync.Mutex
	calls int
}

func (m *streamThenErrorClient) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == 1 {
		return m.first, nil
	}
	return nil, m.err
}

// TestClawBackend_NudgeRetryableErrorPropagates verifies the FINDING-3
// guard composes with the network-resilience retry: when the nudge re-run
// itself fails transiently (a mid-stream drop — the exact class the
// truncation work classifies retryable), the error must PROPAGATE so the
// outer retry loop re-issues the turn, NOT be swallowed back into the
// degenerate first-pass result that the recovery pass would then coerce
// into a placeholder verdict.
func TestClawBackend_NudgeRetryableErrorPropagates(t *testing.T) {
	reg := NewRegistry()
	mock := &streamThenErrorClient{
		first: mockStreamEvents("I will review the diff.", "end_turn"), // narration, 0 tools → guard fires
		err:   &APIError{Message: "incomplete stream: connection closed", IsRetryable: true},
	}
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })

	schema := &ir.Schema{Name: "verdict", Fields: []*ir.SchemaField{{Name: "approved", Type: ir.FieldTypeBool}}}
	schemaJSON, _ := SchemaToJSON(schema)
	toolDefs := []delegate.ToolDef{{
		Name:        "bash",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { return "", nil },
	}}

	// MaxAttempts:1 → no retry, so the propagated error surfaces directly.
	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
	_, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "reviewer_gpt",
		Model:        "test/test-model",
		UserPrompt:   "Review.",
		OutputSchema: schemaJSON,
		HasTools:     true,
		ToolDefs:     toolDefs,
		ToolMaxSteps: 5,
	})
	if err == nil {
		t.Fatal("expected the retryable nudge error to propagate, got nil (it was swallowed into a recovery verdict)")
	}
	if !isRetryable(err) {
		t.Errorf("propagated error must stay retryable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "nudge re-run") {
		t.Errorf("error should identify the nudge re-run as the source, got: %v", err)
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
	if req.System != "You are a helpful assistant." {
		t.Errorf("req.System = %q, want mirrored system prompt", req.System)
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

// scriptedClient returns a different StreamResponse stream per call,
// allowing tests to simulate multi-turn conversations where each turn
// produces a different LLM response. The captured CreateMessageRequest
// for each call is recorded for later assertion.
type scriptedClient struct {
	requests []api.CreateMessageRequest
	calls    int
	scripts  [][]api.StreamEvent
}

func (c *scriptedClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.requests = append(c.requests, req)
	idx := c.calls
	c.calls++
	if idx >= len(c.scripts) {
		idx = len(c.scripts) - 1
	}
	events := c.scripts[idx]
	ch := make(chan api.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func newScriptedBackend(scripts ...[]api.StreamEvent) (*ClawBackend, *scriptedClient) {
	reg := NewRegistry()
	sc := &scriptedClient{scripts: scripts}
	reg.Register("test", func(modelID string) (api.APIClient, error) { return sc, nil })
	return NewClawBackend(reg, EventHooks{}, RetryPolicy{}), sc
}

// askUserToolDef returns a ToolDef whose handler always raises
// *delegate.ErrAskUser carrying the question argument verbatim. Used by
// L1/L3 tests to drive the ask_user tool loop without depending on the
// real claw-code-go ask_user implementation.
func askUserToolDef() delegate.ToolDef {
	return delegate.ToolDef{
		Name:        "ask_user",
		Description: "Ask the human a question",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"}},"required":["question"]}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Question string `json:"question"`
			}
			_ = json.Unmarshal(input, &args)
			return "", &delegate.ErrAskUser{Question: args.Question}
		},
	}
}

// TestClawBackend_ResumeConversationReplacesMessages verifies the L1
// resume path: when Task.ResumeConversation carries the persisted
// pre-pause history and Task.ResumePendingToolUseID names a pending
// tool_use, the wire-level request's Messages slice is the persisted
// conversation followed by a single user-role message containing a
// tool_result block answering the pending call. The original UserPrompt
// is ignored because the conversation already contains it.
func TestClawBackend_ResumeConversationReplacesMessages(t *testing.T) {
	backend, cap := newCapturingBackend()

	persisted := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "kick off the work"}}},
		{Role: "assistant", Content: []api.ContentBlock{{
			Type:  "tool_use",
			ID:    "toolu_99",
			Name:  "ask_user",
			Input: map[string]any{"question": "Which env?"},
		}}},
	}
	convBytes, err := json.Marshal(persisted)
	if err != nil {
		t.Fatalf("marshal persisted: %v", err)
	}

	_, err = backend.Execute(context.Background(), delegate.Task{
		NodeID:                 "agent1",
		Model:                  "test/test-model",
		UserPrompt:             "this should be ignored on resume",
		ResumeConversation:     convBytes,
		ResumePendingToolUseID: "toolu_99",
		ResumeAnswer:           "staging",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(cap.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(cap.requests))
	}
	msgs := cap.requests[0].Messages
	if len(msgs) != 3 {
		t.Fatalf("Messages len = %d, want 3 (2 persisted + tool_result), got %+v", len(msgs), msgs)
	}
	// First two messages must be the persisted history verbatim.
	if msgs[0].Role != "user" || len(msgs[0].Content) == 0 || msgs[0].Content[0].Text != "kick off the work" {
		t.Errorf("msgs[0] = %+v, want persisted user prompt", msgs[0])
	}
	if msgs[1].Role != "assistant" || len(msgs[1].Content) == 0 || msgs[1].Content[0].Type != "tool_use" || msgs[1].Content[0].ID != "toolu_99" {
		t.Errorf("msgs[1] = %+v, want persisted assistant tool_use(toolu_99)", msgs[1])
	}
	// Final message: user with a tool_result block answering toolu_99.
	if msgs[2].Role != "user" || len(msgs[2].Content) != 1 {
		t.Fatalf("msgs[2] = %+v, want single tool_result content block", msgs[2])
	}
	tr := msgs[2].Content[0]
	if tr.Type != "tool_result" {
		t.Errorf("tr.Type = %q, want tool_result", tr.Type)
	}
	if tr.ToolUseID != "toolu_99" {
		t.Errorf("tr.ToolUseID = %q, want %q", tr.ToolUseID, "toolu_99")
	}
	// Content is either a string ("staging") or nested blocks; assert the
	// answer text is present somewhere in the serialized content.
	bodyJSON, _ := json.Marshal(tr)
	if !strings.Contains(string(bodyJSON), "staging") {
		t.Errorf("tool_result content does not contain answer %q: %s", "staging", bodyJSON)
	}
}

// TestMaybeCompactPause verifies the helper is a no-op for short
// transcripts and produces a bounded continuation message + the
// preserve-recent window for long ones. The pending tool_use must
// always remain addressable.
func TestMaybeCompactPause(t *testing.T) {
	// Short transcript: returned unchanged.
	short := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "tu_1", Name: "ask_user", Input: map[string]any{"question": "?"}}}},
	}
	got := maybeCompactPause(short, "", 0, 0)
	if len(got) != len(short) {
		t.Errorf("short transcript was unexpectedly compacted: %d → %d", len(short), len(got))
	}

	// Long transcript: build many user/assistant turns each with
	// substantial text content so the token estimator goes over the
	// 10_000 default threshold. The final assistant message is the
	// pending tool_use we want to preserve.
	bigText := strings.Repeat("filler word ", 200) // ~2400 chars per message
	long := make([]api.Message, 0, 30)
	for i := 0; i < 28; i++ {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		long = append(long, api.Message{
			Role:    role,
			Content: []api.ContentBlock{{Type: "text", Text: bigText}},
		})
	}
	long = append(long, api.Message{
		Role: "assistant",
		Content: []api.ContentBlock{{
			Type: "tool_use", ID: "tu_pending", Name: "ask_user",
			Input: map[string]any{"question": "Continue?"},
		}},
	})

	got = maybeCompactPause(long, "", 0, 0)
	if len(got) >= len(long) {
		t.Fatalf("long transcript not compacted: input %d, got %d", len(long), len(got))
	}
	rawBytes, _ := json.Marshal(got)
	origBytes, _ := json.Marshal(long)
	if len(rawBytes) >= len(origBytes) {
		t.Errorf("compacted size %d not smaller than original %d", len(rawBytes), len(origBytes))
	}
	// The pending tool_use must survive in the preserved-recent window.
	if !strings.Contains(string(rawBytes), "tu_pending") {
		t.Errorf("compacted conversation lost the pending tool_use ID: %s", rawBytes)
	}
}

// TestClawBackend_MultiPauseAccumulatesHistory verifies L3 (multi-turn
// ask_user accumulation) emerges for free from L1: when the LLM calls
// ask_user a second time after the first answer is delivered, the
// conversation persisted at the second pause must contain BOTH the
// original Q1/A1 exchange AND the new Q2 tool_use. Without this
// property, only the most recent question would survive across pauses
// and a third resume would lose context for everything before.
func TestClawBackend_MultiPauseAccumulatesHistory(t *testing.T) {
	// First call: model issues tool_use(ask_user, Q1).
	firstScript := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 10},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_q1", Name: "ask_user"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"question":"Which env? Q1"}`}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 5}},
		{Type: api.EventMessageStop},
	}
	// Second call (after resume with A1): model issues tool_use(ask_user, Q2).
	secondScript := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 20},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_q2", Name: "ask_user"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"question":"Which region? Q2"}`}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 5}},
		{Type: api.EventMessageStop},
	}

	backend, _ := newScriptedBackend(firstScript, secondScript)

	// First Execute: should yield a paused Result with PendingConversation.
	firstResult, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "agent1",
		Model:        "test/test-model",
		UserPrompt:   "do the work",
		HasTools:     true,
		ToolDefs:     []delegate.ToolDef{askUserToolDef()},
		ToolMaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if needs, _ := firstResult.Output["_needs_interaction"].(bool); !needs {
		t.Fatalf("first call did not produce _needs_interaction: %v", firstResult.Output)
	}
	if firstResult.PendingToolUseID != "tu_q1" {
		t.Errorf("first PendingToolUseID = %q, want tu_q1", firstResult.PendingToolUseID)
	}
	if len(firstResult.PendingConversation) == 0 {
		t.Fatal("first PendingConversation is empty")
	}

	// Second Execute (resume): pass the persisted conversation + answer.
	secondResult, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:                 "agent1",
		Model:                  "test/test-model",
		UserPrompt:             "ignored on resume",
		HasTools:               true,
		ToolDefs:               []delegate.ToolDef{askUserToolDef()},
		ToolMaxSteps:           5,
		ResumeConversation:     firstResult.PendingConversation,
		ResumePendingToolUseID: firstResult.PendingToolUseID,
		ResumeAnswer:           "answer-A1",
	})
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if needs, _ := secondResult.Output["_needs_interaction"].(bool); !needs {
		t.Fatalf("second call did not produce _needs_interaction: %v", secondResult.Output)
	}
	if secondResult.PendingToolUseID != "tu_q2" {
		t.Errorf("second PendingToolUseID = %q, want tu_q2", secondResult.PendingToolUseID)
	}

	// L3: the second PendingConversation must include Q1, A1, and Q2.
	var msgs []api.Message
	if err := json.Unmarshal(secondResult.PendingConversation, &msgs); err != nil {
		t.Fatalf("unmarshal second conversation: %v", err)
	}
	// Expect: [user:original, assistant:tool_use(tu_q1), user:tool_result(tu_q1, A1), assistant:tool_use(tu_q2)]
	if len(msgs) != 4 {
		t.Fatalf("conversation len = %d, want 4 (original, Q1, A1, Q2); got: %+v", len(msgs), msgs)
	}
	full, _ := json.Marshal(msgs)
	for _, want := range []string{"do the work", "tu_q1", "answer-A1", "tu_q2", "Which region? Q2"} {
		if !strings.Contains(string(full), want) {
			t.Errorf("accumulated conversation missing %q: %s", want, full)
		}
	}
}
