package api_test

import (
	internalapi "claw-code-go/internal/api"
	"claw-code-go/pkg/api"
	"testing"
)

// TestTypeIdentity verifies that pkg/api types are true aliases of
// internal/api types — not copies. The assignment below compiles only
// if the types are identical (type aliases, not new types).
func TestTypeIdentity(t *testing.T) {
	// Assign internal types to pkg types — must compile with zero conversion.
	var _ api.Tool = internalapi.Tool{
		Name:        "test",
		Description: "a test tool",
		InputSchema: internalapi.InputSchema{
			Type:       "object",
			Properties: map[string]internalapi.Property{"x": {Type: "string", Description: "x"}},
		},
	}

	var _ api.Message = internalapi.Message{
		Role: "user",
		Content: []internalapi.ContentBlock{
			{Type: "text", Text: "hello"},
		},
	}

	var _ api.CreateMessageRequest = internalapi.CreateMessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
	}

	var _ api.StreamEvent = internalapi.StreamEvent{
		Type: internalapi.EventContentBlockDelta,
	}

	var _ api.AuthSource = internalapi.APIKeyAuth("test-key")
}

// TestConstructTypes verifies that core types can be constructed through
// the public package and their fields work as expected.
func TestConstructTypes(t *testing.T) {
	tool := api.Tool{
		Name:        "bash",
		Description: "Run a shell command",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"command": {Type: "string", Description: "The command to run"},
			},
			Required: []string{"command"},
		},
	}
	if tool.Name != "bash" {
		t.Errorf("expected tool name 'bash', got %q", tool.Name)
	}

	msg := api.Message{
		Role: "user",
		Content: []api.ContentBlock{
			{Type: "text", Text: "hello"},
		},
	}
	if msg.Role != "user" {
		t.Errorf("expected role 'user', got %q", msg.Role)
	}

	req := api.CreateMessageRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 4096,
		Messages:  []api.Message{msg},
		Tools:     []api.Tool{tool},
	}
	if req.Model != "claude-sonnet-4-20250514" {
		t.Errorf("unexpected model: %s", req.Model)
	}
	if len(req.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(req.Tools))
	}
}

// TestSseParser verifies that NewSseParser and Push work through the shim.
func TestSseParser(t *testing.T) {
	parser := api.NewSseParser()
	if parser == nil {
		t.Fatal("NewSseParser returned nil")
	}

	// Push a complete SSE frame
	chunk := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
	events, err := parser.Push(chunk)
	if err != nil {
		t.Fatalf("Push error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != api.EventContentBlockDelta {
		t.Errorf("expected event type %q, got %q", api.EventContentBlockDelta, events[0].Type)
	}
	if events[0].Delta.Text != "hello" {
		t.Errorf("expected delta text 'hello', got %q", events[0].Delta.Text)
	}
}

// TestNewClient verifies that NewClient works through the shim.
func TestNewClient(t *testing.T) {
	client := api.NewClient("test-key", "claude-sonnet-4-20250514")
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	if client.APIKey != "test-key" {
		t.Errorf("expected API key 'test-key', got %q", client.APIKey)
	}
	if client.Model != "claude-sonnet-4-20250514" {
		t.Errorf("expected model 'claude-sonnet-4-20250514', got %q", client.Model)
	}
}

// TestConstants verifies that re-exported constants match internal values.
func TestConstants(t *testing.T) {
	if api.EventMessageStart != "message_start" {
		t.Errorf("EventMessageStart = %q", api.EventMessageStart)
	}
	if api.EventError != "error" {
		t.Errorf("EventError = %q", api.EventError)
	}
	if api.AuthMethodAPIKey != "api_key" {
		t.Errorf("AuthMethodAPIKey = %q", api.AuthMethodAPIKey)
	}
	if api.AuthSourceNone != 0 {
		t.Errorf("AuthSourceNone = %d", api.AuthSourceNone)
	}
}

// TestAuthFunctions verifies that auth function wrappers work.
func TestAuthFunctions(t *testing.T) {
	none := api.NoAuth()
	if none.Kind != api.AuthSourceNone {
		t.Errorf("NoAuth().Kind = %d, want %d", none.Kind, api.AuthSourceNone)
	}

	key := api.APIKeyAuth("sk-test")
	if key.Kind != api.AuthSourceAPIKey {
		t.Errorf("APIKeyAuth().Kind = %d, want %d", key.Kind, api.AuthSourceAPIKey)
	}
	if key.APIKey != "sk-test" {
		t.Errorf("APIKeyAuth().APIKey = %q", key.APIKey)
	}

	bearer := api.BearerAuth("token-123")
	if bearer.Kind != api.AuthSourceBearer {
		t.Errorf("BearerAuth().Kind = %d", bearer.Kind)
	}

	combined := api.CombinedAuth("sk-test", "token-123")
	if combined.Kind != api.AuthSourceCombined {
		t.Errorf("CombinedAuth().Kind = %d", combined.Kind)
	}
}

// TestEphemeralCacheControl verifies the var wrapper works.
func TestEphemeralCacheControl(t *testing.T) {
	cc := api.EphemeralCacheControl()
	if cc == nil {
		t.Fatal("EphemeralCacheControl returned nil")
	}
	if cc.Type != "ephemeral" {
		t.Errorf("expected type 'ephemeral', got %q", cc.Type)
	}
}
