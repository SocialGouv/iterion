package model

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	goai "github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"

	"github.com/iterion-ai/iterion/ir"
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

	tools := map[string]goai.Tool{
		"git_diff": {
			Name: "git_diff",
			Execute: func(_ context.Context, input json.RawMessage) (string, error) {
				return `{"diff":"+ new line"}`, nil
			},
		},
	}

	exec := NewGoaiExecutor(reg, wf, WithToolImplementations(tools))

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

	tools := map[string]goai.Tool{
		"echo": {
			Name: "echo",
			Execute: func(_ context.Context, input json.RawMessage) (string, error) {
				return "plain text output", nil
			},
		},
	}

	exec := NewGoaiExecutor(reg, wf, WithToolImplementations(tools))

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
