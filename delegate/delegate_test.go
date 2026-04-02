package delegate

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRegistryResolve(t *testing.T) {
	r := NewRegistry()
	r.Register("test_backend", &mockBackend{})

	_, err := r.Resolve("test_backend")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = r.Resolve("unknown")
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestDefaultRegistry(t *testing.T) {
	r := DefaultRegistry()

	_, err := r.Resolve("claude_code")
	if err != nil {
		t.Fatalf("claude_code not found: %v", err)
	}

	_, err = r.Resolve("codex")
	if err != nil {
		t.Fatalf("codex not found: %v", err)
	}
}

func TestParseJSONOutput_Object(t *testing.T) {
	data := []byte(`{"approved": true, "summary": "looks good"}`)
	result, err := parseJSONOutput(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Output["approved"] != true {
		t.Errorf("expected approved=true, got %v", result.Output["approved"])
	}
	if result.Output["summary"] != "looks good" {
		t.Errorf("expected summary='looks good', got %v", result.Output["summary"])
	}
}

func TestParseJSONOutput_TextFallback(t *testing.T) {
	data := []byte(`This is plain text output.`)
	result, err := parseJSONOutput(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Output["text"] != "This is plain text output." {
		t.Errorf("unexpected text: %v", result.Output["text"])
	}
}

func TestParseJSONOutput_Empty(t *testing.T) {
	result, err := parseJSONOutput([]byte(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Output) != 0 {
		t.Errorf("expected empty output, got %v", result.Output)
	}
}

func TestParseJSONOutput_ClaudeArray(t *testing.T) {
	// Simulate claude --output-format json array output.
	arr := []map[string]interface{}{
		{
			"type": "message",
			"role": "user",
			"content": []map[string]interface{}{
				{"type": "text", "text": "Do something"},
			},
		},
		{
			"type": "message",
			"role": "assistant",
			"content": []map[string]interface{}{
				{"type": "text", "text": `{"approved": false, "issues": ["bug found"]}`},
			},
		},
	}
	data, _ := json.Marshal(arr)
	result, err := parseJSONOutput(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Output["approved"] != false {
		t.Errorf("expected approved=false, got %v", result.Output["approved"])
	}
}

func TestParseClaudeResult_ParseFallbackDetection(t *testing.T) {
	// When the result is plain text, the output has only a "text" key.
	data := []byte(`{"type":"result","result":"just plain text","usage":{"input_tokens":10,"output_tokens":5}}`)
	result, err := parseClaudeResult(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Output["text"] != "just plain text" {
		t.Errorf("expected text fallback, got %v", result.Output)
	}
	// Parse functions don't set ParseFallback — that's the backend's job
	// based on whether OutputSchema was set.
	if result.ParseFallback {
		t.Error("parse function should not set ParseFallback")
	}
	if len(result.Output) != 1 {
		t.Errorf("expected exactly 1 key in text fallback output, got %d", len(result.Output))
	}
}

func TestParseClaudeResult_StructuredOutput(t *testing.T) {
	data := []byte(`{"type":"result","result":"{\"approved\":true}","usage":{"input_tokens":10,"output_tokens":5}}`)
	result, err := parseClaudeResult(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Output["approved"] != true {
		t.Errorf("expected approved=true, got %v", result.Output)
	}
	if result.Tokens != 15 {
		t.Errorf("expected 15 tokens, got %d", result.Tokens)
	}
}

func TestParseCodexJSONL_ParseFallbackDetection(t *testing.T) {
	// Codex JSONL with plain text agent message.
	data := []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"just plain text"}}
{"type":"turn.completed","usage":{"input_tokens":20,"output_tokens":10}}`)
	result, err := parseCodexJSONL(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Output["text"] != "just plain text" {
		t.Errorf("expected text fallback, got %v", result.Output)
	}
	if result.ParseFallback {
		t.Error("parse function should not set ParseFallback")
	}
	if result.Tokens != 30 {
		t.Errorf("expected 30 tokens, got %d", result.Tokens)
	}
}

func TestParseCodexJSONL_StructuredOutput(t *testing.T) {
	data := []byte(`{"type":"item.completed","item":{"type":"agent_message","text":"{\"verdict\":\"pass\"}"}}
{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":3}}`)
	result, err := parseCodexJSONL(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Output["verdict"] != "pass" {
		t.Errorf("expected verdict=pass, got %v", result.Output)
	}
}

func TestResult_NewFieldsZeroFromParse(t *testing.T) {
	// Verify that parse functions return zero-valued metadata fields
	// (backends populate them in Execute, not in parse).
	data := []byte(`{"approved": true}`)
	result, err := parseJSONOutput(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Duration != 0 {
		t.Error("Duration should be zero from parse function")
	}
	if result.ExitCode != 0 {
		t.Error("ExitCode should be zero from parse function")
	}
	if result.Stderr != "" {
		t.Error("Stderr should be empty from parse function")
	}
	if result.BackendName != "" {
		t.Error("BackendName should be empty from parse function")
	}
	if result.RawOutputLen != 0 {
		t.Error("RawOutputLen should be zero from parse function")
	}
	if result.ParseFallback {
		t.Error("ParseFallback should be false from parse function")
	}
}

// mockBackend implements Backend for testing.
type mockBackend struct {
	response Result
	err      error
}

func (m *mockBackend) Execute(_ context.Context, _ Task) (Result, error) {
	return m.response, m.err
}
