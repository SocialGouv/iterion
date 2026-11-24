package runtime

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/claw/internal/lsp"
	"github.com/SocialGouv/iterion/pkg/claw/internal/mcp"
	"testing"
)

// TestBatch3ToolDispatch verifies that all 13 new Batch 3 tools dispatch
// through ExecuteTool without panicking or returning "unknown tool" errors.
func TestBatch3ToolDispatch(t *testing.T) {
	loop := NewConversationLoop(&Config{Model: "test"}, nil)
	loop.LspRegistry = lsp.NewRegistry()
	loop.MCPRegistry = mcp.NewRegistry()
	loop.McpAuthState = mcp.NewAuthState()

	// Worker tools — all dispatch to WorkerRegistry, which is already initialized.
	workerTools := []struct {
		name  string
		input map[string]any
	}{
		{"worker_create", map[string]any{"cwd": "/tmp"}},
		{"worker_get", map[string]any{"worker_id": "nonexistent"}},
		{"worker_observe", map[string]any{"worker_id": "nonexistent", "screen_text": "text"}},
		{"worker_resolve_trust", map[string]any{"worker_id": "nonexistent"}},
		{"worker_await_ready", map[string]any{"worker_id": "nonexistent"}},
		{"worker_send_prompt", map[string]any{"worker_id": "nonexistent"}},
		{"worker_restart", map[string]any{"worker_id": "nonexistent"}},
		{"worker_terminate", map[string]any{"worker_id": "nonexistent"}},
		{"worker_observe_completion", map[string]any{"worker_id": "nonexistent", "finish_reason": "stop", "tokens_output": float64(0)}},
	}

	for _, tt := range workerTools {
		t.Run(tt.name, func(t *testing.T) {
			cb := loop.ExecuteTool(context.Background(), tt.name, tt.input)
			if cb.Type != "tool_result" {
				t.Errorf("expected tool_result type, got %q", cb.Type)
			}
			// worker_create should succeed; others may error because of nonexistent ID.
			if tt.name == "worker_create" && cb.IsError {
				t.Errorf("worker_create should succeed, got error: %v", cb.Content)
			}
		})
	}

	// LSP tool.
	t.Run("lsp", func(t *testing.T) {
		cb := loop.ExecuteTool(context.Background(), "lsp", map[string]any{"action": "diagnostics"})
		if cb.Type != "tool_result" {
			t.Errorf("expected tool_result type, got %q", cb.Type)
		}
		// Should not be an error — diagnostics with no servers returns empty.
		if cb.IsError {
			t.Errorf("lsp diagnostics should not error, got: %v", cb.Content)
		}
	})

	// MCP resource/auth tools.
	mcpTools := []struct {
		name  string
		input map[string]any
	}{
		{"list_mcp_resources", map[string]any{"server": "nonexistent"}},
		{"read_mcp_resource", map[string]any{"uri": "test://x", "server": "nonexistent"}},
		{"mcp_auth", map[string]any{"server": "nonexistent"}},
	}

	for _, tt := range mcpTools {
		t.Run(tt.name, func(t *testing.T) {
			cb := loop.ExecuteTool(context.Background(), tt.name, tt.input)
			if cb.Type != "tool_result" {
				t.Errorf("expected tool_result type, got %q", cb.Type)
			}
			// These return structured error JSON (not Go errors) for missing servers.
			if cb.IsError {
				t.Errorf("%s should return structured error, not Go error: %v", tt.name, cb.Content)
			}
		})
	}
}

// TestBatch3WorkerCreateDispatch verifies a successful worker_create through the full loop.
func TestBatch3WorkerCreateDispatch(t *testing.T) {
	loop := NewConversationLoop(&Config{Model: "test"}, nil)
	cb := loop.ExecuteTool(context.Background(), "worker_create", map[string]any{"cwd": "/tmp"})
	if cb.IsError {
		t.Fatalf("worker_create dispatch failed: %v", cb.Content)
	}
	if len(cb.Content) == 0 || cb.Content[0].Text == "" {
		t.Fatal("expected non-empty result from worker_create")
	}
}

// TestBatch3NilRegistries verifies graceful errors when registries are nil.
func TestBatch3NilRegistries(t *testing.T) {
	loop := &ConversationLoop{
		Permissions:    DefaultPermissions(),
		Config:         &Config{Model: "test"},
		WorkerRegistry: nil, // Intentionally nil.
		LspRegistry:    nil,
		MCPRegistry:    nil,
		McpAuthState:   nil,
	}

	// The MCP resource/auth tools never hard-error on nil registries:
	// mcpProvider() falls back to a default Provider so they answer with
	// uniform "not found"/"disconnected" shapes instead (see mcpProvider).
	nilTests := []struct {
		name      string
		input     map[string]any
		wantError bool
	}{
		{"worker_create", map[string]any{"cwd": "/tmp"}, true},
		{"lsp", map[string]any{"action": "diagnostics"}, true},
		{"list_mcp_resources", map[string]any{}, false},
		{"read_mcp_resource", map[string]any{"uri": "test://x"}, false},
		{"mcp_auth", map[string]any{"server": "test"}, false},
	}

	for _, tt := range nilTests {
		t.Run(tt.name+"_nil_registry", func(t *testing.T) {
			cb := loop.ExecuteTool(context.Background(), tt.name, tt.input)
			if cb.Type != "tool_result" {
				t.Errorf("expected tool_result type, got %q", cb.Type)
			}
			if cb.IsError != tt.wantError {
				t.Errorf("IsError = %v, want %v for %s (nil registry)", cb.IsError, tt.wantError, tt.name)
			}
		})
	}
}

// TestBatch3ToolCount verifies the tool list includes all 13 new tools.
func TestBatch3ToolCount(t *testing.T) {
	loop := NewConversationLoop(&Config{Model: "test"}, nil)

	batch3Names := map[string]bool{
		"worker_create":             true,
		"worker_get":                true,
		"worker_observe":            true,
		"worker_resolve_trust":      true,
		"worker_await_ready":        true,
		"worker_send_prompt":        true,
		"worker_restart":            true,
		"worker_terminate":          true,
		"worker_observe_completion": true,
		"lsp":                       true,
		"list_mcp_resources":        true,
		"read_mcp_resource":         true,
		"mcp_auth":                  true,
	}

	toolNames := make(map[string]bool)
	for _, tool := range loop.Tools {
		toolNames[tool.Name] = true
	}

	for name := range batch3Names {
		if !toolNames[name] {
			t.Errorf("tool %q not found in Tools slice", name)
		}
	}
}
