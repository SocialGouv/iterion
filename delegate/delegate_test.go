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

// mockBackend implements Backend for testing.
type mockBackend struct {
	response Result
	err      error
}

func (m *mockBackend) Execute(_ context.Context, _ Task) (Result, error) {
	return m.response, m.err
}
