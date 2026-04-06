package model

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	goai "github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/tool"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// mockModel is a test double for provider.LanguageModel.
type mockModel struct {
	id       string
	response *provider.GenerateResult
	err      error
}

func (m *mockModel) ModelID() string { return m.id }

func (m *mockModel) DoGenerate(_ context.Context, _ provider.GenerateParams) (*provider.GenerateResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *mockModel) DoStream(_ context.Context, _ provider.GenerateParams) (*provider.StreamResult, error) {
	return nil, fmt.Errorf("streaming not implemented in mock")
}

// failThenSucceedModel fails the first N calls with a retryable error,
// then succeeds with the given response.
type failThenSucceedModel struct {
	id       string
	response *provider.GenerateResult
	failures int // how many times to fail before succeeding
	mu       sync.Mutex
	calls    int
}

func (m *failThenSucceedModel) ModelID() string { return m.id }

func (m *failThenSucceedModel) DoGenerate(_ context.Context, _ provider.GenerateParams) (*provider.GenerateResult, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()

	if call <= m.failures {
		return nil, &goai.APIError{
			Message:     "rate limited",
			StatusCode:  429,
			IsRetryable: true,
		}
	}
	return m.response, nil
}

func (m *failThenSucceedModel) DoStream(_ context.Context, _ provider.GenerateParams) (*provider.StreamResult, error) {
	return nil, fmt.Errorf("streaming not implemented in mock")
}

// alwaysFailModel always fails with a retryable error.
type alwaysFailModel struct {
	id         string
	statusCode int
	mu         sync.Mutex
	calls      int
}

func (m *alwaysFailModel) ModelID() string { return m.id }

func (m *alwaysFailModel) DoGenerate(_ context.Context, _ provider.GenerateParams) (*provider.GenerateResult, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	return nil, &goai.APIError{
		Message:     fmt.Sprintf("server error %d", m.statusCode),
		StatusCode:  m.statusCode,
		IsRetryable: true,
	}
}

func (m *alwaysFailModel) DoStream(_ context.Context, _ provider.GenerateParams) (*provider.StreamResult, error) {
	return nil, fmt.Errorf("streaming not implemented in mock")
}

// capableMockModel adds Capabilities to mockModel.
type capableMockModel struct {
	mockModel
	caps provider.ModelCapabilities
}

func (m *capableMockModel) Capabilities() provider.ModelCapabilities {
	return m.caps
}

// ---------------------------------------------------------------------------
// Registry tests
// ---------------------------------------------------------------------------

func TestParseModelSpec(t *testing.T) {
	tests := []struct {
		spec     string
		provider string
		model    string
		wantErr  bool
	}{
		{"anthropic/claude-sonnet-4-20250514", "anthropic", "claude-sonnet-4-20250514", false},
		{"openai/gpt-4o", "openai", "gpt-4o", false},
		{"google/gemini-2.0-flash", "google", "gemini-2.0-flash", false},
		{"no-slash", "", "", true},
		{"/model", "", "", true},
		{"provider/", "", "", true},
	}

	for _, tt := range tests {
		p, m, err := ParseModelSpec(tt.spec)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseModelSpec(%q): err=%v, wantErr=%v", tt.spec, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if p != tt.provider || m != tt.model {
				t.Errorf("ParseModelSpec(%q): got (%q, %q), want (%q, %q)", tt.spec, p, m, tt.provider, tt.model)
			}
		}
	}
}

func TestRegistryResolve(t *testing.T) {
	r := NewRegistry()

	mock := &mockModel{id: "test-model"}
	r.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	// Valid spec.
	m, err := r.Resolve("test/test-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ModelID() != "test-model" {
		t.Errorf("got model ID %q, want %q", m.ModelID(), "test-model")
	}

	// Same spec returns cached model.
	m2, err := r.Resolve("test/test-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m != m2 {
		t.Error("expected cached model, got different instance")
	}

	// Unknown provider.
	_, err = r.Resolve("unknown/model")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}

	// Invalid spec.
	_, err = r.Resolve("no-slash")
	if err == nil {
		t.Fatal("expected error for invalid spec")
	}
}

func TestRegistryCapabilities(t *testing.T) {
	r := NewRegistry()
	mock := &capableMockModel{
		mockModel: mockModel{id: "capable-model"},
		caps: provider.ModelCapabilities{
			ToolCall:    true,
			Temperature: true,
		},
	}
	r.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	caps, err := r.Capabilities("test/capable-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !caps.ToolCall {
		t.Error("expected ToolCall capability")
	}
	if !caps.Temperature {
		t.Error("expected Temperature capability")
	}
}

// ---------------------------------------------------------------------------
// Schema tests
// ---------------------------------------------------------------------------

func TestSchemaToJSON(t *testing.T) {
	schema := &ir.Schema{
		Name: "verdict",
		Fields: []*ir.SchemaField{
			{Name: "verdict", Type: ir.FieldTypeBool},
			{Name: "reason", Type: ir.FieldTypeString},
			{Name: "score", Type: ir.FieldTypeFloat},
			{Name: "tags", Type: ir.FieldTypeStringArray},
			{Name: "status", Type: ir.FieldTypeString, EnumValues: []string{"pass", "fail"}},
		},
	}

	raw, err := SchemaToJSON(schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if parsed["type"] != "object" {
		t.Errorf("expected type=object, got %v", parsed["type"])
	}

	props, ok := parsed["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("missing properties")
	}
	if len(props) != 5 {
		t.Errorf("expected 5 properties, got %d", len(props))
	}

	// Verify field types.
	assertPropType(t, props, "verdict", "boolean")
	assertPropType(t, props, "reason", "string")
	assertPropType(t, props, "score", "number")
	assertPropType(t, props, "tags", "array")

	// Verify enum.
	status := props["status"].(map[string]interface{})
	enumVals, ok := status["enum"].([]interface{})
	if !ok {
		t.Fatal("missing enum for status")
	}
	if len(enumVals) != 2 {
		t.Errorf("expected 2 enum values, got %d", len(enumVals))
	}

	// Verify required.
	req, ok := parsed["required"].([]interface{})
	if !ok {
		t.Fatal("missing required")
	}
	if len(req) != 5 {
		t.Errorf("expected 5 required, got %d", len(req))
	}

	// Verify additionalProperties.
	if parsed["additionalProperties"] != false {
		t.Error("expected additionalProperties=false")
	}
}

func TestSchemaToJSONNil(t *testing.T) {
	_, err := SchemaToJSON(nil)
	if err == nil {
		t.Fatal("expected error for nil schema")
	}
}

func assertPropType(t *testing.T, props map[string]interface{}, field, expectedType string) {
	t.Helper()
	p, ok := props[field].(map[string]interface{})
	if !ok {
		t.Errorf("missing property %q", field)
		return
	}
	if p["type"] != expectedType {
		t.Errorf("property %q: got type %v, want %q", field, p["type"], expectedType)
	}
}

// ---------------------------------------------------------------------------
// Executor tests
// ---------------------------------------------------------------------------

func TestExecuteLLMTextGeneration(t *testing.T) {
	reg := NewRegistry()
	mock := &mockModel{
		id: "test-model",
		response: &provider.GenerateResult{
			Text:         "This is the review.",
			FinishReason: provider.FinishStop,
			Usage:        provider.Usage{InputTokens: 100, OutputTokens: 50},
		},
	}
	reg.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{
			"system_review": {
				Name: "system_review",
				Body: "You are a code reviewer. Review the following diff.",
			},
		},
		Schemas: map[string]*ir.Schema{},
	}

	exec := NewGoaiExecutor(reg, wf)

	node := &ir.Node{
		ID:           "reviewer",
		Kind:         ir.NodeAgent,
		Model:        "test/test-model",
		SystemPrompt: "system_review",
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"diff": "some diff content",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if output["text"] != "This is the review." {
		t.Errorf("got text %q, want %q", output["text"], "This is the review.")
	}
	if output["_tokens"] != 150 {
		t.Errorf("got tokens %v, want 150", output["_tokens"])
	}
	if output["_model"] != "test-model" {
		t.Errorf("got model %v, want %q", output["_model"], "test-model")
	}
}

func TestExecuteLLMStructuredOutput(t *testing.T) {
	reg := NewRegistry()
	mock := &mockModel{
		id: "test-model",
		response: &provider.GenerateResult{
			Text:         `{"verdict":true,"reason":"Looks good"}`,
			FinishReason: provider.FinishStop,
			Usage:        provider.Usage{InputTokens: 100, OutputTokens: 30},
		},
	}
	reg.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{
			"verdict_schema": {
				Name: "verdict_schema",
				Fields: []*ir.SchemaField{
					{Name: "verdict", Type: ir.FieldTypeBool},
					{Name: "reason", Type: ir.FieldTypeString},
				},
			},
		},
	}

	exec := NewGoaiExecutor(reg, wf)

	node := &ir.Node{
		ID:           "judge",
		Kind:         ir.NodeJudge,
		Model:        "test/test-model",
		OutputSchema: "verdict_schema",
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"review": "code looks clean",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if output["verdict"] != true {
		t.Errorf("got verdict %v, want true", output["verdict"])
	}
	if output["reason"] != "Looks good" {
		t.Errorf("got reason %q, want %q", output["reason"], "Looks good")
	}
	if output["_tokens"] != 130 {
		t.Errorf("got tokens %v, want 130", output["_tokens"])
	}
}

func TestExecutorEventHooks(t *testing.T) {
	reg := NewRegistry()
	mock := &mockModel{
		id: "test-model",
		response: &provider.GenerateResult{
			Text:         "result",
			FinishReason: provider.FinishStop,
			Usage:        provider.Usage{InputTokens: 10, OutputTokens: 5},
		},
	}
	reg.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	var requestNodeID, responseNodeID string
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := NewGoaiExecutor(reg, wf, WithEventHooks(EventHooks{
		OnLLMRequest: func(nodeID string, info goai.RequestInfo) {
			requestNodeID = nodeID
		},
		OnLLMResponse: func(nodeID string, info goai.ResponseInfo) {
			responseNodeID = nodeID
		},
	}))

	node := &ir.Node{
		ID:    "agent1",
		Kind:  ir.NodeAgent,
		Model: "test/test-model",
	}

	_, err := exec.Execute(context.Background(), node, map[string]interface{}{"prompt": "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if requestNodeID != "agent1" {
		t.Errorf("OnLLMRequest: got nodeID %q, want %q", requestNodeID, "agent1")
	}
	if responseNodeID != "agent1" {
		t.Errorf("OnLLMResponse: got nodeID %q, want %q", responseNodeID, "agent1")
	}
}

func TestExecutorToolNode(t *testing.T) {
	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	tr := tool.NewRegistry()
	_ = tr.RegisterBuiltin("git_diff", "Show git diff", nil, func(_ context.Context, _ json.RawMessage) (string, error) {
		return `{"diff":"+ new line"}`, nil
	})

	exec := NewGoaiExecutor(reg, wf, WithToolRegistry(tr))

	node := &ir.Node{
		ID:      "get_diff",
		Kind:    ir.NodeTool,
		Command: "git_diff",
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"branch": "feature",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if output["diff"] != "+ new line" {
		t.Errorf("got diff %v, want %q", output["diff"], "+ new line")
	}
}

func TestExecutorToolNodeTextOutput(t *testing.T) {
	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	tr := tool.NewRegistry()
	_ = tr.RegisterBuiltin("echo", "Echo tool", nil, func(_ context.Context, _ json.RawMessage) (string, error) {
		return "plain text output", nil
	})

	exec := NewGoaiExecutor(reg, wf, WithToolRegistry(tr))

	node := &ir.Node{
		ID:      "run_echo",
		Kind:    ir.NodeTool,
		Command: "echo",
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if output["result"] != "plain text output" {
		t.Errorf("got result %v, want %q", output["result"], "plain text output")
	}
}

func TestExecutorUnknownModel(t *testing.T) {
	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := NewGoaiExecutor(reg, wf)

	node := &ir.Node{
		ID:    "agent",
		Kind:  ir.NodeAgent,
		Model: "unknown/model",
	}

	_, err := exec.Execute(context.Background(), node, map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}

// ---------------------------------------------------------------------------
// Retry tests
// ---------------------------------------------------------------------------

func TestRetryOnTransientError(t *testing.T) {
	reg := NewRegistry()
	mock := &failThenSucceedModel{
		id: "test-model",
		response: &provider.GenerateResult{
			Text:         "success after retry",
			FinishReason: provider.FinishStop,
			Usage:        provider.Usage{InputTokens: 10, OutputTokens: 5},
		},
		failures: 2, // fail twice, succeed on 3rd attempt
	}
	reg.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	var retries []RetryInfo
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := NewGoaiExecutor(reg, wf,
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
		WithEventHooks(EventHooks{
			OnLLMRetry: func(nodeID string, info RetryInfo) {
				retries = append(retries, info)
			},
		}),
	)

	node := &ir.Node{
		ID:    "agent1",
		Kind:  ir.NodeAgent,
		Model: "test/test-model",
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{"prompt": "hello"})
	if err != nil {
		t.Fatalf("expected success after retries, got error: %v", err)
	}
	if output["text"] != "success after retry" {
		t.Errorf("got text %q, want %q", output["text"], "success after retry")
	}

	// Should have 2 retry events.
	if len(retries) != 2 {
		t.Fatalf("expected 2 retry events, got %d", len(retries))
	}
	if retries[0].Attempt != 1 {
		t.Errorf("retry[0].Attempt = %d, want 1", retries[0].Attempt)
	}
	if retries[0].StatusCode != 429 {
		t.Errorf("retry[0].StatusCode = %d, want 429", retries[0].StatusCode)
	}
	if retries[1].Attempt != 2 {
		t.Errorf("retry[1].Attempt = %d, want 2", retries[1].Attempt)
	}
}

func TestRetryExhausted(t *testing.T) {
	reg := NewRegistry()
	mock := &alwaysFailModel{id: "test-model", statusCode: 500}
	reg.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	var retries []RetryInfo
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := NewGoaiExecutor(reg, wf,
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
		WithEventHooks(EventHooks{
			OnLLMRetry: func(nodeID string, info RetryInfo) {
				retries = append(retries, info)
			},
		}),
	)

	node := &ir.Node{
		ID:    "agent1",
		Kind:  ir.NodeAgent,
		Model: "test/test-model",
	}

	_, err := exec.Execute(context.Background(), node, map[string]interface{}{"prompt": "hello"})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}

	// 3 attempts = 2 retries.
	if len(retries) != 2 {
		t.Fatalf("expected 2 retry events, got %d", len(retries))
	}

	// Total calls should be 3.
	mock.mu.Lock()
	calls := mock.calls
	mock.mu.Unlock()
	if calls != 3 {
		t.Errorf("expected 3 total calls, got %d", calls)
	}
}

func TestNoRetryOnNonRetryableError(t *testing.T) {
	reg := NewRegistry()
	mock := &mockModel{
		id:  "test-model",
		err: fmt.Errorf("non-retryable error"),
	}
	reg.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	var retries []RetryInfo
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := NewGoaiExecutor(reg, wf,
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
		WithEventHooks(EventHooks{
			OnLLMRetry: func(nodeID string, info RetryInfo) {
				retries = append(retries, info)
			},
		}),
	)

	node := &ir.Node{
		ID:    "agent1",
		Kind:  ir.NodeAgent,
		Model: "test/test-model",
	}

	_, err := exec.Execute(context.Background(), node, map[string]interface{}{"prompt": "hello"})
	if err == nil {
		t.Fatal("expected error")
	}

	// No retries for non-retryable errors.
	if len(retries) != 0 {
		t.Errorf("expected 0 retry events, got %d", len(retries))
	}
}

func TestRetryOnStructuredOutput(t *testing.T) {
	reg := NewRegistry()
	mock := &failThenSucceedModel{
		id: "test-model",
		response: &provider.GenerateResult{
			Text:         `{"verdict":true,"reason":"OK"}`,
			FinishReason: provider.FinishStop,
			Usage:        provider.Usage{InputTokens: 50, OutputTokens: 20},
		},
		failures: 1,
	}
	reg.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	var retryCount int
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{
			"verdict_schema": {
				Name: "verdict_schema",
				Fields: []*ir.SchemaField{
					{Name: "verdict", Type: ir.FieldTypeBool},
					{Name: "reason", Type: ir.FieldTypeString},
				},
			},
		},
	}

	exec := NewGoaiExecutor(reg, wf,
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
		WithEventHooks(EventHooks{
			OnLLMRetry: func(nodeID string, info RetryInfo) {
				retryCount++
			},
		}),
	)

	node := &ir.Node{
		ID:           "judge",
		Kind:         ir.NodeJudge,
		Model:        "test/test-model",
		OutputSchema: "verdict_schema",
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"review": "code",
	})
	if err != nil {
		t.Fatalf("expected success after retry, got error: %v", err)
	}
	if output["verdict"] != true {
		t.Errorf("got verdict %v, want true", output["verdict"])
	}
	if retryCount != 1 {
		t.Errorf("expected 1 retry, got %d", retryCount)
	}
}

func TestRetryContextCancellation(t *testing.T) {
	reg := NewRegistry()
	mock := &alwaysFailModel{id: "test-model", statusCode: 429}
	reg.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := NewGoaiExecutor(reg, wf,
		WithRetryPolicy(RetryPolicy{MaxAttempts: 10, BackoffBase: time.Second}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately to abort the retry backoff.
	cancel()

	node := &ir.Node{
		ID:    "agent1",
		Kind:  ir.NodeAgent,
		Model: "test/test-model",
	}

	_, err := exec.Execute(ctx, node, map[string]interface{}{"prompt": "hello"})
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// ---------------------------------------------------------------------------
// Structured output validation tests
// ---------------------------------------------------------------------------

func TestStructuredOutputMissingField(t *testing.T) {
	reg := NewRegistry()
	// Response missing the "reason" field.
	mock := &mockModel{
		id: "test-model",
		response: &provider.GenerateResult{
			Text:         `{"verdict":true}`,
			FinishReason: provider.FinishStop,
			Usage:        provider.Usage{InputTokens: 50, OutputTokens: 20},
		},
	}
	reg.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{
			"verdict_schema": {
				Name: "verdict_schema",
				Fields: []*ir.SchemaField{
					{Name: "verdict", Type: ir.FieldTypeBool},
					{Name: "reason", Type: ir.FieldTypeString},
				},
			},
		},
	}

	exec := NewGoaiExecutor(reg, wf)

	node := &ir.Node{
		ID:           "judge",
		Kind:         ir.NodeJudge,
		Model:        "test/test-model",
		OutputSchema: "verdict_schema",
	}

	_, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"review": "code",
	})
	if err == nil {
		t.Fatal("expected error for missing required field")
	}
	if !contains([]string{err.Error()}, "") {
		// Just verify the error mentions the issue.
		t.Logf("got expected error: %v", err)
	}
}

func TestStructuredOutputWrongType(t *testing.T) {
	reg := NewRegistry()
	// Return a string where a bool is expected.
	mock := &mockModel{
		id: "test-model",
		response: &provider.GenerateResult{
			Text:         `{"verdict":"yes","reason":"OK"}`,
			FinishReason: provider.FinishStop,
			Usage:        provider.Usage{InputTokens: 50, OutputTokens: 20},
		},
	}
	reg.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{
			"verdict_schema": {
				Name: "verdict_schema",
				Fields: []*ir.SchemaField{
					{Name: "verdict", Type: ir.FieldTypeBool},
					{Name: "reason", Type: ir.FieldTypeString},
				},
			},
		},
	}

	exec := NewGoaiExecutor(reg, wf)

	node := &ir.Node{
		ID:           "judge",
		Kind:         ir.NodeJudge,
		Model:        "test/test-model",
		OutputSchema: "verdict_schema",
	}

	_, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"review": "code",
	})
	if err == nil {
		t.Fatal("expected error for wrong field type")
	}
	t.Logf("got expected error: %v", err)
}

func TestStructuredOutputInvalidEnum(t *testing.T) {
	reg := NewRegistry()
	mock := &mockModel{
		id: "test-model",
		response: &provider.GenerateResult{
			Text:         `{"status":"maybe"}`,
			FinishReason: provider.FinishStop,
			Usage:        provider.Usage{InputTokens: 50, OutputTokens: 20},
		},
	}
	reg.Register("test", func(modelID string) (provider.LanguageModel, error) {
		return mock, nil
	})

	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{
			"status_schema": {
				Name: "status_schema",
				Fields: []*ir.SchemaField{
					{Name: "status", Type: ir.FieldTypeString, EnumValues: []string{"pass", "fail"}},
				},
			},
		},
	}

	exec := NewGoaiExecutor(reg, wf)

	node := &ir.Node{
		ID:           "judge",
		Kind:         ir.NodeJudge,
		Model:        "test/test-model",
		OutputSchema: "status_schema",
	}

	_, err := exec.Execute(context.Background(), node, map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error for invalid enum value")
	}
	t.Logf("got expected error: %v", err)
}

// ---------------------------------------------------------------------------
// Validate output unit tests
// ---------------------------------------------------------------------------

func TestValidateOutputValid(t *testing.T) {
	schema := &ir.Schema{
		Name: "test",
		Fields: []*ir.SchemaField{
			{Name: "text", Type: ir.FieldTypeString},
			{Name: "ok", Type: ir.FieldTypeBool},
			{Name: "count", Type: ir.FieldTypeInt},
			{Name: "score", Type: ir.FieldTypeFloat},
			{Name: "tags", Type: ir.FieldTypeStringArray},
			{Name: "meta", Type: ir.FieldTypeJSON},
		},
	}

	output := map[string]interface{}{
		"text":  "hello",
		"ok":    true,
		"count": float64(42),
		"score": 3.14,
		"tags":  []interface{}{"a", "b"},
		"meta":  map[string]interface{}{"key": "value"},
	}

	if err := ValidateOutput(output, schema); err != nil {
		t.Errorf("expected valid output, got: %v", err)
	}
}

func TestValidateOutputMissingField(t *testing.T) {
	schema := &ir.Schema{
		Name:   "test",
		Fields: []*ir.SchemaField{{Name: "required_field", Type: ir.FieldTypeString}},
	}

	err := ValidateOutput(map[string]interface{}{}, schema)
	if err == nil {
		t.Fatal("expected error for missing field")
	}
}

func TestValidateOutputNullField(t *testing.T) {
	schema := &ir.Schema{
		Name:   "test",
		Fields: []*ir.SchemaField{{Name: "val", Type: ir.FieldTypeString}},
	}

	err := ValidateOutput(map[string]interface{}{"val": nil}, schema)
	if err == nil {
		t.Fatal("expected error for null field")
	}
}

func TestValidateOutputEnumViolation(t *testing.T) {
	schema := &ir.Schema{
		Name: "test",
		Fields: []*ir.SchemaField{
			{Name: "status", Type: ir.FieldTypeString, EnumValues: []string{"pass", "fail"}},
		},
	}

	err := ValidateOutput(map[string]interface{}{"status": "maybe"}, schema)
	if err == nil {
		t.Fatal("expected error for invalid enum")
	}
}

func TestValidateOutputIntegerCheck(t *testing.T) {
	schema := &ir.Schema{
		Name:   "test",
		Fields: []*ir.SchemaField{{Name: "count", Type: ir.FieldTypeInt}},
	}

	// Whole float is OK.
	if err := ValidateOutput(map[string]interface{}{"count": float64(42)}, schema); err != nil {
		t.Errorf("expected 42.0 to be valid integer, got: %v", err)
	}

	// Non-whole float is not.
	if err := ValidateOutput(map[string]interface{}{"count": 3.14}, schema); err == nil {
		t.Error("expected error for non-integer float")
	}
}

func TestValidateOutputStringArrayBadElement(t *testing.T) {
	schema := &ir.Schema{
		Name:   "test",
		Fields: []*ir.SchemaField{{Name: "tags", Type: ir.FieldTypeStringArray}},
	}

	err := ValidateOutput(map[string]interface{}{"tags": []interface{}{"ok", 42}}, schema)
	if err == nil {
		t.Fatal("expected error for non-string array element")
	}
}

// ---------------------------------------------------------------------------
// Template resolution tests
// ---------------------------------------------------------------------------

func TestResolveTemplate(t *testing.T) {
	exec := &GoaiExecutor{
		vars: map[string]interface{}{
			"rules": "Be thorough",
		},
	}

	input := map[string]interface{}{
		"diff": "file.go: +func Hello()",
	}

	body := "Review this PR:\n{{input.diff}}\nRules: {{vars.rules}}"
	result := exec.resolveTemplate(body, input)

	expected := "Review this PR:\nfile.go: +func Hello()\nRules: Be thorough"
	if result != expected {
		t.Errorf("got:\n%s\nwant:\n%s", result, expected)
	}
}

func TestResolveTemplateUnknownRef(t *testing.T) {
	exec := &GoaiExecutor{}

	result := exec.resolveTemplate("Hello {{unknown.ref}}", nil)
	if result != "Hello {{unknown.ref}}" {
		t.Errorf("expected unresolved ref to remain, got %q", result)
	}
}

func TestResolveTemplateJSONValue(t *testing.T) {
	exec := &GoaiExecutor{}

	input := map[string]interface{}{
		"items": []string{"a", "b", "c"},
	}

	result := exec.resolveTemplate("Items: {{input.items}}", input)
	if result != `Items: ["a","b","c"]` {
		t.Errorf("got %q", result)
	}
}

func TestSetVars(t *testing.T) {
	exec := &GoaiExecutor{}
	vars := map[string]interface{}{"key": "value"}
	exec.SetVars(vars)

	if exec.vars["key"] != "value" {
		t.Errorf("SetVars did not set vars correctly")
	}
}

// ---------------------------------------------------------------------------
// Reasoning effort resolution
// ---------------------------------------------------------------------------

func TestResolveReasoningEffort(t *testing.T) {
	tests := []struct {
		name     string
		node     *ir.Node
		input    map[string]interface{}
		expected string
	}{
		{
			name:     "static only",
			node:     &ir.Node{ReasoningEffort: "high"},
			input:    map[string]interface{}{},
			expected: "high",
		},
		{
			name:     "dynamic override",
			node:     &ir.Node{ReasoningEffort: "medium"},
			input:    map[string]interface{}{"_reasoning_effort": "low"},
			expected: "low",
		},
		{
			name:     "dynamic extra_high",
			node:     &ir.Node{ReasoningEffort: "low"},
			input:    map[string]interface{}{"_reasoning_effort": "extra_high"},
			expected: "extra_high",
		},
		{
			name:     "invalid dynamic falls back to static",
			node:     &ir.Node{ReasoningEffort: "high"},
			input:    map[string]interface{}{"_reasoning_effort": "ultra"},
			expected: "high",
		},
		{
			name:     "no value set",
			node:     &ir.Node{},
			input:    map[string]interface{}{},
			expected: "",
		},
		{
			name:     "dynamic non-string ignored",
			node:     &ir.Node{ReasoningEffort: "medium"},
			input:    map[string]interface{}{"_reasoning_effort": 42},
			expected: "medium",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveReasoningEffort(tt.node, tt.input)
			if got != tt.expected {
				t.Errorf("resolveReasoningEffort() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestProviderOptsForNode(t *testing.T) {
	if opts := providerOptsForNode(""); opts != nil {
		t.Errorf("expected nil for empty effort, got %v", opts)
	}

	opts := providerOptsForNode("high")
	if opts == nil {
		t.Fatal("expected non-nil opts for effort 'high'")
	}
	if opts["reasoning_effort"] != "high" {
		t.Errorf("expected reasoning_effort 'high', got %v", opts["reasoning_effort"])
	}
}
