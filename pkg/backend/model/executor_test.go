package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// mockStreamEvents builds a channel of api.StreamEvent that simulates a
// complete Anthropic streaming response with the given text and stop reason.
func mockStreamEvents(text string, stopReason string) <-chan api.StreamEvent {
	ch := make(chan api.StreamEvent, 10)
	go func() {
		defer close(ch)
		// message_start
		ch <- api.StreamEvent{Type: api.EventMessageStart, InputTokens: 100}
		// content_block_start
		ch <- api.StreamEvent{
			Type:         api.EventContentBlockStart,
			ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0},
		}
		// content_block_delta
		ch <- api.StreamEvent{
			Type:  api.EventContentBlockDelta,
			Index: 0,
			Delta: api.Delta{Type: "text_delta", Text: text},
		}
		// content_block_stop
		ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
		// message_delta
		ch <- api.StreamEvent{
			Type:       api.EventMessageDelta,
			StopReason: stopReason,
			Usage:      api.UsageDelta{OutputTokens: 50},
		}
		// message_stop
		ch <- api.StreamEvent{Type: api.EventMessageStop}
	}()
	return ch
}

// execMockClient is a test double for api.APIClient.
type execMockClient struct {
	streams []<-chan api.StreamEvent // responses to return in order
	err     error                    // error to return (overrides streams)
	mu      sync.Mutex
	calls   int
}

func (m *execMockClient) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	idx := m.calls - 1
	if idx >= len(m.streams) {
		idx = len(m.streams) - 1
	}
	return m.streams[idx], nil
}

// failThenSucceedClient fails the first N calls, then returns streams.
type failThenSucceedClient struct {
	streams  []<-chan api.StreamEvent
	failures int
	mu       sync.Mutex
	calls    int
}

func (m *failThenSucceedClient) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()

	if call <= m.failures {
		return nil, &APIError{
			Message:     "rate limited",
			StatusCode:  429,
			IsRetryable: true,
		}
	}
	idx := call - m.failures - 1
	if idx >= len(m.streams) {
		idx = len(m.streams) - 1
	}
	return m.streams[idx], nil
}

// clawErrThenSucceedClient fails the first N calls with a claw-code-go
// public *api.APIError (the type returned by provider HTTP clients on
// non-2xx responses), then returns streams. Used to verify that
// iterion's retry loop classifies clawAPIError the same way it
// classifies its local *APIError.
type clawErrThenSucceedClient struct {
	streams  []<-chan api.StreamEvent
	err      *api.APIError
	failures int
	mu       sync.Mutex
	calls    int
}

func (m *clawErrThenSucceedClient) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()

	if call <= m.failures {
		return nil, m.err
	}
	idx := call - m.failures - 1
	if idx >= len(m.streams) {
		idx = len(m.streams) - 1
	}
	return m.streams[idx], nil
}

// alwaysFailClient always returns a retryable error.
type alwaysFailClient struct {
	statusCode int
	mu         sync.Mutex
	calls      int
}

func (m *alwaysFailClient) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return nil, &APIError{
		Message:     fmt.Sprintf("server error %d", m.statusCode),
		StatusCode:  m.statusCode,
		IsRetryable: true,
	}
}

// newTestClawExecutor creates a ClawExecutor with the claw backend pre-registered.
func newTestClawExecutor(reg *Registry, wf *ir.Workflow, opts ...ClawExecutorOption) *ClawExecutor {
	e := NewClawExecutor(reg, wf, opts...)
	e.backendRegistry.Register(delegate.BackendClaw, NewClawBackend(reg, e.hooks, e.retry))
	return e
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

	mock := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("hello", "end_turn")}}
	r.Register("test", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	// Valid spec.
	m, err := r.Resolve("test/test-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("expected non-nil client")
	}

	// Same spec returns cached client.
	m2, err := r.Resolve("test/test-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m != m2 {
		t.Error("expected cached client, got different instance")
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
	mock := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("hello", "end_turn")}}
	r.Register("test", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	// Static capabilities for unknown provider → defaults.
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
// Capabilities tests
// ---------------------------------------------------------------------------

func TestCapabilitiesForModel(t *testing.T) {
	tests := []struct {
		provider string
		modelID  string
		wantCaps ModelCapabilities
	}{
		{"anthropic", "claude-sonnet-4-20250514", ModelCapabilities{Reasoning: true, ToolCall: true, Temperature: true}},
		{"anthropic", "claude-3-haiku", ModelCapabilities{Reasoning: false, ToolCall: true, Temperature: true}},
		{"openai", "gpt-4o", ModelCapabilities{Reasoning: false, ToolCall: true, Temperature: true}},
		{"openai", "o1-preview", ModelCapabilities{Reasoning: true, ToolCall: true, Temperature: false}},
		{"openai", "o3-mini", ModelCapabilities{Reasoning: true, ToolCall: true, Temperature: false}},
		{"unknown", "any-model", ModelCapabilities{Reasoning: false, ToolCall: true, Temperature: true}},
	}
	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.modelID, func(t *testing.T) {
			caps := capabilitiesForModel(tt.provider, tt.modelID)
			if caps != tt.wantCaps {
				t.Errorf("capabilitiesForModel(%q, %q) = %+v, want %+v", tt.provider, tt.modelID, caps, tt.wantCaps)
			}
		})
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

	assertPropType(t, props, "verdict", "boolean")
	assertPropType(t, props, "reason", "string")
	assertPropType(t, props, "score", "number")
	assertPropType(t, props, "tags", "array")

	status := props["status"].(map[string]interface{})
	enumVals, ok := status["enum"].([]interface{})
	if !ok {
		t.Fatal("missing enum for status")
	}
	if len(enumVals) != 2 {
		t.Errorf("expected 2 enum values, got %d", len(enumVals))
	}

	req, ok := parsed["required"].([]interface{})
	if !ok {
		t.Fatal("missing required")
	}
	if len(req) != 5 {
		t.Errorf("expected 5 required, got %d", len(req))
	}

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
	mock := &execMockClient{
		streams: []<-chan api.StreamEvent{mockStreamEvents("This is the review.", "end_turn")},
	}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
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

	exec := newTestClawExecutor(reg, wf)

	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "reviewer"},
		LLMFields: ir.LLMFields{Model: "test/test-model", SystemPrompt: "system_review"},
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
	if output["_model"] != "test/test-model" {
		t.Errorf("got model %v, want %q", output["_model"], "test/test-model")
	}
}

func TestExecuteLLMStructuredOutput(t *testing.T) {
	reg := NewRegistry()
	// For structured output, the model returns a tool_use block.
	ch := make(chan api.StreamEvent, 10)
	go func() {
		defer close(ch)
		ch <- api.StreamEvent{Type: api.EventMessageStart, InputTokens: 100}
		ch <- api.StreamEvent{
			Type:  api.EventContentBlockStart,
			Index: 0,
			ContentBlock: api.ContentBlockInfo{
				Type:  "tool_use",
				Index: 0,
				ID:    "tu_1",
				Name:  "structured_output",
			},
		}
		ch <- api.StreamEvent{
			Type:  api.EventContentBlockDelta,
			Index: 0,
			Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"verdict":true,"reason":"Looks good"}`},
		}
		ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
		ch <- api.StreamEvent{
			Type:       api.EventMessageDelta,
			StopReason: "tool_use",
			Usage:      api.UsageDelta{OutputTokens: 30},
		}
		ch <- api.StreamEvent{Type: api.EventMessageStop}
	}()

	mock := &execMockClient{streams: []<-chan api.StreamEvent{ch}}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
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

	exec := newTestClawExecutor(reg, wf)

	node := &ir.JudgeNode{
		BaseNode:     ir.BaseNode{ID: "judge"},
		LLMFields:    ir.LLMFields{Model: "test/test-model"},
		SchemaFields: ir.SchemaFields{OutputSchema: "verdict_schema"},
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
	mock := &execMockClient{
		streams: []<-chan api.StreamEvent{mockStreamEvents("result", "end_turn")},
	}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	var requestNodeID, responseNodeID string
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := newTestClawExecutor(reg, wf, WithEventHooks(EventHooks{
		OnLLMRequest: func(nodeID string, info LLMRequestInfo) {
			requestNodeID = nodeID
		},
		OnLLMResponse: func(nodeID string, info LLMResponseInfo) {
			responseNodeID = nodeID
		},
	}))

	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "agent1"},
		LLMFields: ir.LLMFields{Model: "test/test-model"},
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

	exec := newTestClawExecutor(reg, wf, WithToolRegistry(tr))

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "get_diff"},
		Command:  "git_diff",
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

	exec := newTestClawExecutor(reg, wf, WithToolRegistry(tr))

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "run_echo"},
		Command:  "echo",
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if output["result"] != "plain text output" {
		t.Errorf("got result %v, want %q", output["result"], "plain text output")
	}
}

func TestExecutorToolNodeShellCommand(t *testing.T) {
	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := newTestClawExecutor(reg, wf)

	refs, err := ir.ParseRefs("echo {{input.message}}")
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}

	node := &ir.ToolNode{
		BaseNode:    ir.BaseNode{ID: "commit_tool"},
		Command:     "echo {{input.message}}",
		CommandRefs: refs,
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"message": "hello world",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if output["result"] != "hello world" {
		t.Errorf("got result %v, want %q", output["result"], "hello world")
	}
}

func TestExecutorToolNodeShellMultipleRefs(t *testing.T) {
	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := newTestClawExecutor(reg, wf)

	refs, err := ir.ParseRefs("echo {{input.name}} {{input.value}}")
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}

	node := &ir.ToolNode{
		BaseNode:    ir.BaseNode{ID: "multi_ref"},
		Command:     "echo {{input.name}} {{input.value}}",
		CommandRefs: refs,
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"name":  "foo",
		"value": "bar",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if output["result"] != "foo bar" {
		t.Errorf("got result %v, want %q", output["result"], "foo bar")
	}
}

func TestExecutorToolNodeShellJSONOutput(t *testing.T) {
	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := newTestClawExecutor(reg, wf)

	refs, err := ir.ParseRefs(`printf '{"status":"%s"}' {{input.status}}`)
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}

	node := &ir.ToolNode{
		BaseNode:    ir.BaseNode{ID: "json_tool"},
		Command:     `printf '{"status":"%s"}' {{input.status}}`,
		CommandRefs: refs,
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"status": "ok",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if output["status"] != "ok" {
		t.Errorf("got status %v, want %q", output["status"], "ok")
	}
}

func TestExecutorToolNodeShellInjection(t *testing.T) {
	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := newTestClawExecutor(reg, wf)

	refs, err := ir.ParseRefs("echo {{input.msg}}")
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}

	node := &ir.ToolNode{
		BaseNode:    ir.BaseNode{ID: "inject_tool"},
		Command:     "echo {{input.msg}}",
		CommandRefs: refs,
	}

	malicious := "'; echo INJECTED; echo '"
	output, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"msg": malicious,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, _ := output["result"].(string)
	if result != malicious {
		t.Errorf("shell escaping failed: got %q, want literal %q", result, malicious)
	}
}

// TestExecutorToolNodeEnvVarInjectionAfterEscape is a regression test for
// the security bug where os.ExpandEnv was applied AFTER shellEscape, allowing
// an upstream-controlled input value of `$VAR` to be expanded into shell
// metacharacters that escaped from the protective single quotes. With the
// fix, ExpandEnv runs on the author's command template only; substituted
// values containing `$VAR` are quoted as the literal string `$VAR`.
func TestExecutorToolNodeEnvVarInjectionAfterEscape(t *testing.T) {
	// The "attacker" sets an env var whose value contains shell
	// metacharacters that, if interpreted, would close the protective
	// single quotes and execute an arbitrary command (here: write a
	// sentinel file we can later check for absence).
	sentinel := filepath.Join(t.TempDir(), "pwned")
	t.Setenv("ITERION_TEST_INJECT", "'; touch "+sentinel+"; echo '")

	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}
	exec := newTestClawExecutor(reg, wf)

	refs, err := ir.ParseRefs("echo {{input.msg}}")
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}
	node := &ir.ToolNode{
		BaseNode:    ir.BaseNode{ID: "inject_env_tool"},
		Command:     "echo {{input.msg}}",
		CommandRefs: refs,
	}

	// The upstream value is the literal string `$ITERION_TEST_INJECT`.
	// Pre-fix, this would survive shellEscape as `'$ITERION_TEST_INJECT'`,
	// then ExpandEnv would substitute the env value, producing
	// `''; touch <sentinel>; echo ''` which sh would interpret as three
	// commands: an empty string, a `touch`, and another empty string.
	payload := "$ITERION_TEST_INJECT"
	output, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"msg": payload,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1. The sentinel file must NOT exist — the touch payload was neutralized.
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Fatalf("SECURITY REGRESSION: sentinel file %q exists, payload was executed (stat err: %v)", sentinel, statErr)
	}

	// 2. The echo output should be the LITERAL `$ITERION_TEST_INJECT` string,
	//    proving that the env var was not expanded inside the substituted value.
	result, _ := output["result"].(string)
	if result != payload {
		t.Errorf("env var was expanded inside escaped substitution: got %q, want literal %q", result, payload)
	}
}

// TestExecutorToolNodeEnvVarInTemplateStillExpands ensures the legitimate
// use case (env vars in the author's command template) still works after
// the ExpandEnv re-ordering. Workflows like rust_to_go_port.iter rely on
// `export PATH=...:$PATH && cd {{input.dir}} && go build` — the $PATH in
// the static template must still be expanded.
func TestExecutorToolNodeEnvVarInTemplateStillExpands(t *testing.T) {
	t.Setenv("ITERION_TEST_TEMPLATE_VAR", "from_env")

	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}
	exec := newTestClawExecutor(reg, wf)

	refs, err := ir.ParseRefs("echo $ITERION_TEST_TEMPLATE_VAR {{input.suffix}}")
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}
	node := &ir.ToolNode{
		BaseNode:    ir.BaseNode{ID: "tmpl_env_tool"},
		Command:     "echo $ITERION_TEST_TEMPLATE_VAR {{input.suffix}}",
		CommandRefs: refs,
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"suffix": "tail",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _ := output["result"].(string)
	want := "from_env tail"
	if got != want {
		t.Errorf("template env-var expansion regressed: got %q, want %q", got, want)
	}
}

func TestResolveCommandTemplate(t *testing.T) {
	tests := []struct {
		name    string
		command string
		input   map[string]interface{}
		want    string
	}{
		{
			name:    "single ref",
			command: "echo {{input.msg}}",
			input:   map[string]interface{}{"msg": "hello"},
			want:    "echo 'hello'",
		},
		{
			name:    "multiple refs",
			command: "git -C {{input.dir}} commit -m {{input.msg}}",
			input:   map[string]interface{}{"dir": "/tmp/repo", "msg": "feat: port"},
			want:    "git -C '/tmp/repo' commit -m 'feat: port'",
		},
		{
			name:    "missing ref unchanged",
			command: "echo {{input.missing}}",
			input:   map[string]interface{}{},
			want:    "echo {{input.missing}}",
		},
		{
			name:    "no refs passthrough",
			command: "echo hello",
			input:   map[string]interface{}{},
			want:    "echo hello",
		},
		{
			name:    "injection escaped",
			command: "echo {{input.msg}}",
			input:   map[string]interface{}{"msg": "'; rm -rf / #"},
			want:    "echo ''\\''; rm -rf / #'",
		},
		{
			name:    "string slice expands to space-separated args",
			command: "git add -- {{input.files}}",
			input:   map[string]interface{}{"files": []string{"a.go", "b.go"}},
			want:    "git add -- 'a.go' 'b.go'",
		},
		{
			name:    "interface slice (JSON-decoded) expands the same",
			command: "git add -- {{input.files}}",
			input:   map[string]interface{}{"files": []interface{}{"a.go", "b.go"}},
			want:    "git add -- 'a.go' 'b.go'",
		},
		{
			name:    "slice with shell metacharacters stays quoted",
			command: "rm -- {{input.paths}}",
			input:   map[string]interface{}{"paths": []string{"a b.go", "c'd.go", "$HOME/x"}},
			want:    `rm -- 'a b.go' 'c'\''d.go' '$HOME/x'`,
		},
		{
			name:    "empty slice substitutes as empty string",
			command: "git add -- {{input.files}}",
			input:   map[string]interface{}{"files": []string{}},
			want:    "git add -- ",
		},
		{
			name:    "slice with one element",
			command: "echo {{input.files}}",
			input:   map[string]interface{}{"files": []string{"only"}},
			want:    "echo 'only'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := ir.ParseRefs(tt.command)
			if err != nil {
				t.Fatalf("ParseRefs: %v", err)
			}
			got := resolveCommandTemplate(tt.command, refs, tt.input, nil)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExecutorUnknownModel(t *testing.T) {
	reg := NewRegistry()
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := newTestClawExecutor(reg, wf)

	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "agent"},
		LLMFields: ir.LLMFields{Model: "unknown/model"},
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
	mock := &failThenSucceedClient{
		streams:  []<-chan api.StreamEvent{mockStreamEvents("success after retry", "end_turn")},
		failures: 2,
	}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	var retries []RetryInfo
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := newTestClawExecutor(reg, wf,
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
		WithEventHooks(EventHooks{
			OnLLMRetry: func(nodeID string, info RetryInfo) {
				retries = append(retries, info)
			},
		}),
	)

	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "agent1"},
		LLMFields: ir.LLMFields{Model: "test/test-model"},
	}

	output, err := exec.Execute(context.Background(), node, map[string]interface{}{"prompt": "hello"})
	if err != nil {
		t.Fatalf("expected success after retries, got error: %v", err)
	}
	if output["text"] != "success after retry" {
		t.Errorf("got text %q, want %q", output["text"], "success after retry")
	}

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
	mock := &alwaysFailClient{statusCode: 500}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	var retries []RetryInfo
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := newTestClawExecutor(reg, wf,
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
		WithEventHooks(EventHooks{
			OnLLMRetry: func(nodeID string, info RetryInfo) {
				retries = append(retries, info)
			},
		}),
	)

	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "agent1"},
		LLMFields: ir.LLMFields{Model: "test/test-model"},
	}

	_, err := exec.Execute(context.Background(), node, map[string]interface{}{"prompt": "hello"})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}

	if len(retries) != 2 {
		t.Fatalf("expected 2 retry events, got %d", len(retries))
	}

	mock.mu.Lock()
	calls := mock.calls
	mock.mu.Unlock()
	if calls != 3 {
		t.Errorf("expected 3 total calls, got %d", calls)
	}
}

func TestNoRetryOnNonRetryableError(t *testing.T) {
	reg := NewRegistry()
	mock := &execMockClient{
		err: fmt.Errorf("non-retryable error"),
	}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	var retries []RetryInfo
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := newTestClawExecutor(reg, wf,
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
		WithEventHooks(EventHooks{
			OnLLMRetry: func(nodeID string, info RetryInfo) {
				retries = append(retries, info)
			},
		}),
	)

	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "agent1"},
		LLMFields: ir.LLMFields{Model: "test/test-model"},
	}

	_, err := exec.Execute(context.Background(), node, map[string]interface{}{"prompt": "hello"})
	if err == nil {
		t.Fatal("expected error")
	}

	if len(retries) != 0 {
		t.Errorf("expected 0 retry events, got %d", len(retries))
	}
}

func TestRetryOnStructuredOutput(t *testing.T) {
	reg := NewRegistry()

	// First call fails (via failThenSucceedClient), second succeeds with structured output.
	ch := make(chan api.StreamEvent, 10)
	go func() {
		defer close(ch)
		ch <- api.StreamEvent{Type: api.EventMessageStart, InputTokens: 50}
		ch <- api.StreamEvent{
			Type:  api.EventContentBlockStart,
			Index: 0,
			ContentBlock: api.ContentBlockInfo{
				Type:  "tool_use",
				Index: 0,
				ID:    "tu_1",
				Name:  "structured_output",
			},
		}
		ch <- api.StreamEvent{
			Type:  api.EventContentBlockDelta,
			Index: 0,
			Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"verdict":true,"reason":"OK"}`},
		}
		ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
		ch <- api.StreamEvent{
			Type:       api.EventMessageDelta,
			StopReason: "tool_use",
			Usage:      api.UsageDelta{OutputTokens: 20},
		}
		ch <- api.StreamEvent{Type: api.EventMessageStop}
	}()

	mock := &failThenSucceedClient{
		streams:  []<-chan api.StreamEvent{ch},
		failures: 1,
	}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
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

	exec := newTestClawExecutor(reg, wf,
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
		WithEventHooks(EventHooks{
			OnLLMRetry: func(nodeID string, info RetryInfo) {
				retryCount++
			},
		}),
	)

	node := &ir.JudgeNode{
		BaseNode:     ir.BaseNode{ID: "judge"},
		LLMFields:    ir.LLMFields{Model: "test/test-model"},
		SchemaFields: ir.SchemaFields{OutputSchema: "verdict_schema"},
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
	mock := &alwaysFailClient{statusCode: 429}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
		return mock, nil
	})

	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{},
	}

	exec := newTestClawExecutor(reg, wf,
		WithRetryPolicy(RetryPolicy{MaxAttempts: 10, BackoffBase: time.Second}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "agent1"},
		LLMFields: ir.LLMFields{Model: "test/test-model"},
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
			Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"verdict":true}`},
		}
		ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
		ch <- api.StreamEvent{
			Type:       api.EventMessageDelta,
			StopReason: "tool_use",
			Usage:      api.UsageDelta{OutputTokens: 20},
		}
		ch <- api.StreamEvent{Type: api.EventMessageStop}
	}()
	mock := &execMockClient{streams: []<-chan api.StreamEvent{ch}}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
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

	exec := newTestClawExecutor(reg, wf)

	node := &ir.JudgeNode{
		BaseNode:     ir.BaseNode{ID: "judge"},
		LLMFields:    ir.LLMFields{Model: "test/test-model"},
		SchemaFields: ir.SchemaFields{OutputSchema: "verdict_schema"},
	}

	_, err := exec.Execute(context.Background(), node, map[string]interface{}{
		"review": "code",
	})
	if err == nil {
		t.Fatal("expected error for missing required field")
	}
	t.Logf("got expected error: %v", err)
}

func TestStructuredOutputWrongType(t *testing.T) {
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
			Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"verdict":"yes","reason":"OK"}`},
		}
		ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
		ch <- api.StreamEvent{
			Type:       api.EventMessageDelta,
			StopReason: "tool_use",
			Usage:      api.UsageDelta{OutputTokens: 20},
		}
		ch <- api.StreamEvent{Type: api.EventMessageStop}
	}()
	mock := &execMockClient{streams: []<-chan api.StreamEvent{ch}}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
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

	exec := newTestClawExecutor(reg, wf)

	node := &ir.JudgeNode{
		BaseNode:     ir.BaseNode{ID: "judge"},
		LLMFields:    ir.LLMFields{Model: "test/test-model"},
		SchemaFields: ir.SchemaFields{OutputSchema: "verdict_schema"},
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
			Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"status":"maybe"}`},
		}
		ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
		ch <- api.StreamEvent{
			Type:       api.EventMessageDelta,
			StopReason: "tool_use",
			Usage:      api.UsageDelta{OutputTokens: 20},
		}
		ch <- api.StreamEvent{Type: api.EventMessageStop}
	}()
	mock := &execMockClient{streams: []<-chan api.StreamEvent{ch}}
	reg.Register("test", func(modelID string) (api.APIClient, error) {
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

	exec := newTestClawExecutor(reg, wf)

	node := &ir.JudgeNode{
		BaseNode:     ir.BaseNode{ID: "judge"},
		LLMFields:    ir.LLMFields{Model: "test/test-model"},
		SchemaFields: ir.SchemaFields{OutputSchema: "status_schema"},
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

	if err := ValidateOutput(map[string]interface{}{"count": float64(42)}, schema); err != nil {
		t.Errorf("expected 42.0 to be valid integer, got: %v", err)
	}

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
	exec := &ClawExecutor{
		vars: map[string]interface{}{
			"rules": "Be thorough",
		},
	}

	input := map[string]interface{}{
		"diff": "file.go: +func Hello()",
	}

	body := "Review this PR:\n{{input.diff}}\nRules: {{vars.rules}}"
	result := exec.resolveTemplate(body, input, nil)

	expected := "Review this PR:\nfile.go: +func Hello()\nRules: Be thorough"
	if result != expected {
		t.Errorf("got:\n%s\nwant:\n%s", result, expected)
	}
}

func TestResolveTemplateUnknownRef(t *testing.T) {
	exec := &ClawExecutor{}

	result := exec.resolveTemplate("Hello {{unknown.ref}}", nil, nil)
	if result != "Hello {{unknown.ref}}" {
		t.Errorf("expected unresolved ref to remain, got %q", result)
	}
}

func TestResolveTemplateJSONValue(t *testing.T) {
	exec := &ClawExecutor{}

	input := map[string]interface{}{
		"items": []string{"a", "b", "c"},
	}

	result := exec.resolveTemplate("Items: {{input.items}}", input, nil)
	if result != `Items: ["a","b","c"]` {
		t.Errorf("got %q", result)
	}
}

func TestResolveTemplateAttachments(t *testing.T) {
	exec := &ClawExecutor{}
	urlCalls := 0
	td := &TemplateData{
		Attachments: map[string]AttachmentInfo{
			"logo": {
				Name:             "logo",
				Path:             "/store/runs/r1/attachments/logo/logo.png",
				OriginalFilename: "logo.png",
				MIME:             "image/png",
				Size:             4096,
				SHA256:           "abc123",
				PresignURL: func() (string, error) {
					urlCalls++
					return "/api/runs/r1/attachments/logo?sig=xyz", nil
				},
			},
		},
	}
	cases := []struct {
		body string
		want string
	}{
		{"Path: {{attachments.logo}}", "Path: /store/runs/r1/attachments/logo/logo.png"},
		{"Path: {{attachments.logo.path}}", "Path: /store/runs/r1/attachments/logo/logo.png"},
		{"Mime: {{attachments.logo.mime}}", "Mime: image/png"},
		{"Size: {{attachments.logo.size}}", "Size: 4096"},
		{"Hash: {{attachments.logo.sha256}}", "Hash: abc123"},
		{"URL: {{attachments.logo.url}}", "URL: /api/runs/r1/attachments/logo?sig=xyz"},
	}
	for _, tc := range cases {
		got := exec.resolveTemplate(tc.body, nil, td)
		if got != tc.want {
			t.Errorf("body %q: got %q want %q", tc.body, got, tc.want)
		}
	}
	if urlCalls < 1 {
		t.Errorf("PresignURL called %d times; want >=1", urlCalls)
	}
}

func TestResolveTemplateAttachments_Unknown(t *testing.T) {
	exec := &ClawExecutor{}
	td := &TemplateData{Attachments: map[string]AttachmentInfo{}}
	got := exec.resolveTemplate("X={{attachments.missing}}", nil, td)
	if got != "X={{attachments.missing}}" {
		t.Errorf("unknown attachment should pass through, got %q", got)
	}
}

func TestSetVars(t *testing.T) {
	exec := &ClawExecutor{}
	vars := map[string]interface{}{"key": "value"}
	exec.SetVars(vars)

	if exec.vars["key"] != "value" {
		t.Errorf("SetVars did not set vars correctly")
	}
}

// TestNewClawExecutorSeedsVarsDefaults verifies that workflow-declared
// var defaults from the .iter `vars:` block are seeded into the
// executor's vars map at construction, so prompt templates that
// reference {{vars.X}} for an unoverridden var with a default render
// the default value rather than the literal "{{vars.X}}" placeholder.
//
// SetVars must overlay run-level overrides on top of the seeded
// defaults rather than replacing the whole map.
func TestNewClawExecutorSeedsVarsDefaults(t *testing.T) {
	wf := &ir.Workflow{
		Vars: map[string]*ir.Var{
			"scope_notes":   {Name: "scope_notes", Type: ir.VarString, HasDefault: true, Default: ""},
			"workspace_dir": {Name: "workspace_dir", Type: ir.VarString, HasDefault: true, Default: "/default/path"},
			"max_loops":     {Name: "max_loops", Type: ir.VarInt, HasDefault: true, Default: int64(5)},
			"no_default":    {Name: "no_default", Type: ir.VarString, HasDefault: false},
		},
	}
	exec := NewClawExecutor(NewRegistry(), wf)

	// Defaults seeded.
	if got, want := exec.vars["scope_notes"], ""; got != want {
		t.Errorf("scope_notes seed: got %v, want %q", got, want)
	}
	if got, want := exec.vars["workspace_dir"], "/default/path"; got != want {
		t.Errorf("workspace_dir seed: got %v, want %q", got, want)
	}
	if got, want := exec.vars["max_loops"], int64(5); got != want {
		t.Errorf("max_loops seed: got %v, want %v", got, want)
	}
	if _, ok := exec.vars["no_default"]; ok {
		t.Errorf("no_default should not be seeded (HasDefault=false)")
	}

	// SetVars merges on top, preserving non-overridden defaults.
	exec.SetVars(map[string]interface{}{
		"workspace_dir": "/runtime/override",
	})
	if got, want := exec.vars["workspace_dir"], "/runtime/override"; got != want {
		t.Errorf("workspace_dir after override: got %v, want %q", got, want)
	}
	if got, want := exec.vars["scope_notes"], ""; got != want {
		t.Errorf("scope_notes after partial override: got %v, want %q", got, want)
	}
	if got, want := exec.vars["max_loops"], int64(5); got != want {
		t.Errorf("max_loops after partial override: got %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Reasoning effort resolution
// ---------------------------------------------------------------------------

func TestResolveReasoningEffort(t *testing.T) {
	tests := []struct {
		name       string
		nodeEffort string
		input      map[string]interface{}
		expected   string
	}{
		{
			name:       "static only",
			nodeEffort: "high",
			input:      map[string]interface{}{},
			expected:   "high",
		},
		{
			name:       "dynamic override",
			nodeEffort: "medium",
			input:      map[string]interface{}{"_reasoning_effort": "low"},
			expected:   "low",
		},
		{
			name:       "dynamic xhigh",
			nodeEffort: "low",
			input:      map[string]interface{}{"_reasoning_effort": "xhigh"},
			expected:   "xhigh",
		},
		{
			name:       "dynamic max",
			nodeEffort: "low",
			input:      map[string]interface{}{"_reasoning_effort": "max"},
			expected:   "max",
		},
		{
			name:       "invalid dynamic falls back to static",
			nodeEffort: "high",
			input:      map[string]interface{}{"_reasoning_effort": "ultra"},
			expected:   "high",
		},
		{
			name:       "no value set",
			nodeEffort: "",
			input:      map[string]interface{}{},
			expected:   "",
		},
		{
			name:       "dynamic non-string ignored",
			nodeEffort: "medium",
			input:      map[string]interface{}{"_reasoning_effort": 42},
			expected:   "medium",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveReasoningEffort(tt.nodeEffort, tt.input)
			if got != tt.expected {
				t.Errorf("resolveReasoningEffort() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestResolveReasoningEffortEnvSubst covers the env-substituted form
// allowed by the parser: "${VAR}" / "${VAR:-default}". Expansion happens
// at runtime; invalid expansions fall back to the empty string so the
// provider applies its own default.
func TestResolveReasoningEffortEnvSubst(t *testing.T) {
	tests := []struct {
		name       string
		nodeEffort string
		envKey     string
		envValue   string
		input      map[string]interface{}
		expected   string
	}{
		{
			name:       "default fallback when env unset",
			nodeEffort: "${ITERION_TEST_EFFORT:-max}",
			envKey:     "ITERION_TEST_EFFORT",
			envValue:   "",
			expected:   "max",
		},
		{
			name:       "env wins over default",
			nodeEffort: "${ITERION_TEST_EFFORT:-max}",
			envKey:     "ITERION_TEST_EFFORT",
			envValue:   "low",
			expected:   "low",
		},
		{
			name:       "bare env var, set",
			nodeEffort: "${ITERION_TEST_EFFORT}",
			envKey:     "ITERION_TEST_EFFORT",
			envValue:   "high",
			expected:   "high",
		},
		{
			name:       "bare env var, unset, no default → empty (provider falls back)",
			nodeEffort: "${ITERION_TEST_EFFORT}",
			envKey:     "ITERION_TEST_EFFORT",
			envValue:   "",
			expected:   "",
		},
		{
			name:       "invalid expanded value → empty",
			nodeEffort: "${ITERION_TEST_EFFORT:-ultra}",
			envKey:     "ITERION_TEST_EFFORT",
			envValue:   "",
			expected:   "",
		},
		{
			name:       "invalid env value → empty",
			nodeEffort: "${ITERION_TEST_EFFORT:-max}",
			envKey:     "ITERION_TEST_EFFORT",
			envValue:   "ludicrous",
			expected:   "",
		},
		{
			name:       "dynamic input override beats env-substituted default",
			nodeEffort: "${ITERION_TEST_EFFORT:-max}",
			envKey:     "ITERION_TEST_EFFORT",
			envValue:   "low",
			input:      map[string]interface{}{"_reasoning_effort": "high"},
			expected:   "high",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.envKey, tt.envValue)
			input := tt.input
			if input == nil {
				input = map[string]interface{}{}
			}
			got := resolveReasoningEffort(tt.nodeEffort, input)
			if got != tt.expected {
				t.Errorf("resolveReasoningEffort(%q) = %q, want %q",
					tt.nodeEffort, got, tt.expected)
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
