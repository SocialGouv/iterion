package delegate

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
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

	_, err := r.Resolve(BackendClaudeCode)
	if err != nil {
		t.Fatalf("claude_code not found: %v", err)
	}

	_, err = r.Resolve(BackendCodex)
	if err != nil {
		t.Fatalf("codex not found: %v", err)
	}
}

func TestParseSDKOutput_StructuredOutput(t *testing.T) {
	structured := map[string]interface{}{"approved": true, "summary": "looks good"}
	output, rawLen, fallback := parseSDKOutput(nil, structured, nil)
	if output["approved"] != true {
		t.Errorf("expected approved=true, got %v", output["approved"])
	}
	if output["summary"] != "looks good" {
		t.Errorf("expected summary='looks good', got %v", output["summary"])
	}
	if rawLen != 0 {
		t.Errorf("expected rawLen=0 for structured output, got %d", rawLen)
	}
	if fallback {
		t.Error("expected no fallback for structured output")
	}
}

func TestParseSDKOutput_ResultTextJSON(t *testing.T) {
	text := `{"approved": true}`
	output, rawLen, fallback := parseSDKOutput(&text, nil, nil)
	if output["approved"] != true {
		t.Errorf("expected approved=true, got %v", output)
	}
	if rawLen != len(text) {
		t.Errorf("expected rawLen=%d, got %d", len(text), rawLen)
	}
	if fallback {
		t.Error("expected no fallback for JSON text")
	}
}

func TestParseSDKOutput_ResultTextPlain(t *testing.T) {
	text := "This is plain text output."
	output, rawLen, fallback := parseSDKOutput(&text, nil, nil)
	if output["text"] != text {
		t.Errorf("expected text=%q, got %v", text, output["text"])
	}
	if rawLen != len(text) {
		t.Errorf("expected rawLen=%d, got %d", len(text), rawLen)
	}
	if fallback {
		t.Error("expected no fallback when no schema")
	}
}

func TestParseSDKOutput_ResultTextPlainWithSchema(t *testing.T) {
	text := "This is plain text output."
	schema := json.RawMessage(`{"type":"object"}`)
	output, _, fallback := parseSDKOutput(&text, nil, schema)
	if output["text"] != text {
		t.Errorf("expected text=%q, got %v", text, output["text"])
	}
	if !fallback {
		t.Error("expected fallback when schema is set but output is plain text")
	}
}

func TestParseSDKOutput_MarkdownJSON(t *testing.T) {
	text := "Here is the result:\n```json\n{\"verdict\": \"pass\"}\n```"
	output, _, fallback := parseSDKOutput(&text, nil, nil)
	if output["verdict"] != "pass" {
		t.Errorf("expected verdict=pass, got %v", output)
	}
	if fallback {
		t.Error("expected no fallback for markdown JSON")
	}
}

func TestParseSDKOutput_Empty(t *testing.T) {
	output, rawLen, fallback := parseSDKOutput(nil, nil, nil)
	if len(output) != 0 {
		t.Errorf("expected empty output, got %v", output)
	}
	if rawLen != 0 {
		t.Errorf("expected rawLen=0, got %d", rawLen)
	}
	if fallback {
		t.Error("expected no fallback for empty output")
	}
}

func TestParseSDKOutput_StructuredOutputNonMap(t *testing.T) {
	// Structured output that is not a map but can be marshaled to one.
	type result struct {
		Approved bool   `json:"approved"`
		Summary  string `json:"summary"`
	}
	structured := result{Approved: true, Summary: "ok"}
	output, _, fallback := parseSDKOutput(nil, structured, nil)
	if output["approved"] != true {
		t.Errorf("expected approved=true, got %v", output["approved"])
	}
	if fallback {
		t.Error("expected no fallback for struct output")
	}
}

func TestExtractJSONFromMarkdown(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"no fences", "plain text", ""},
		{"json block", "text\n```json\n{\"a\":1}\n```\nmore", `{"a":1}`},
		{"bare block", "```\n{\"b\":2}\n```", `{"b":2}`},
		{"multiple blocks", "```\n{\"first\":1}\n```\n```\n{\"second\":2}\n```", `{"second":2}`},
		{"non-json block", "```\nnot json\n```", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSONFromMarkdown(tt.input)
			if got != tt.expect {
				t.Errorf("extractJSONFromMarkdown(%q) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}

func TestValidateWorkDir(t *testing.T) {
	base := t.TempDir()
	sub := base + "/sub"
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	// Same dir should pass.
	if err := validateWorkDir(base, base); err != nil {
		t.Errorf("expected nil for same dir, got %v", err)
	}
	// Subdir should pass.
	if err := validateWorkDir(sub, base); err != nil {
		t.Errorf("expected nil for subdir, got %v", err)
	}
	// Outside should fail.
	if err := validateWorkDir("/var", base); err == nil {
		t.Error("expected error for outside dir")
	}
	// Empty baseDir should always pass.
	if err := validateWorkDir("/anywhere", ""); err != nil {
		t.Errorf("expected nil for empty baseDir, got %v", err)
	}
}

func TestMapReasoningEffort(t *testing.T) {
	tests := []struct {
		input  string
		expect codexsdk.Effort
	}{
		{"low", codexsdk.EffortLow},
		{"medium", codexsdk.EffortMedium},
		{"high", codexsdk.EffortHigh},
		{"extra_high", codexsdk.EffortMax},
		{"unknown", codexsdk.EffortMedium},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapReasoningEffort(tt.input)
			if got != tt.expect {
				t.Errorf("mapReasoningEffort(%q) = %v, want %v", tt.input, got, tt.expect)
			}
		})
	}
}

func TestFormattingPassUsed_MockBackend(t *testing.T) {
	// Verify that FormattingPassUsed is correctly propagated through Result.
	r := NewRegistry()
	r.Register("mock", &mockBackend{
		response: Result{
			Output:             map[string]interface{}{"approved": true},
			FormattingPassUsed: true,
			BackendName:        "mock",
		},
	})

	backend, err := r.Resolve("mock")
	if err != nil {
		t.Fatal(err)
	}

	result, err := backend.Execute(context.Background(), Task{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.FormattingPassUsed {
		t.Error("expected FormattingPassUsed=true")
	}
	if result.ParseFallback {
		t.Error("expected ParseFallback=false when formatting pass was used")
	}
}

func TestParseSDKOutput_NoFallbackWhenFormattingPassHandles(t *testing.T) {
	// When a two-pass backend returns structured output from Pass 2,
	// parseSDKOutput should return fallback=false since the SDK provides
	// native structured output.
	structured := map[string]interface{}{"verdict": "pass", "score": 9.5}
	schema := json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string"},"score":{"type":"number"}}}`)

	output, _, fallback := parseSDKOutput(nil, structured, schema)
	if fallback {
		t.Error("expected no fallback when SDK returns structured output")
	}
	if output["verdict"] != "pass" {
		t.Errorf("expected verdict=pass, got %v", output["verdict"])
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
