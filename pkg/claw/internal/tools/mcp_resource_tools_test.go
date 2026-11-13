package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/mcp"
)

// mockTransport is a test-only MCP Transport that returns canned responses.
type mockTransport struct {
	mu        sync.Mutex
	responses map[string]any
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		responses: map[string]any{
			"initialize": mcp.InitializeResult{
				ProtocolVersion: "2024-11-05",
				Capabilities: mcp.ServerCapabilities{
					Tools:     &mcp.ToolsCapability{},
					Resources: &mcp.ResourcesCapability{},
				},
				ServerInfo: mcp.ServerInfo{Name: "mock-server", Version: "1.0.0"},
			},
			"tools/list": map[string]any{
				"tools": []mcp.MCPTool{},
			},
			"resources/list": map[string]any{
				"resources": []mcp.McpResourceInfo{
					{URI: "test://doc", Name: "doc", Description: "A test doc", MimeType: "text/plain"},
					{URI: "test://config", Name: "config", Description: "Config file", MimeType: "application/json"},
				},
			},
			"resources/read": map[string]any{
				"contents": []map[string]any{
					{
						"uri":         "test://doc",
						"name":        "doc",
						"description": "A test doc",
						"mimeType":    "text/plain",
						"text":        "Hello, world!",
					},
				},
			},
		},
	}
}

func (m *mockTransport) Send(_ context.Context, req mcp.Request) (mcp.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result, ok := m.responses[req.Method]
	if !ok {
		return mcp.Response{
			JSONRPC: "2.0", ID: req.ID,
			Error: &mcp.RPCError{Code: -32601, Message: fmt.Sprintf("method %q not found", req.Method)},
		}, nil
	}
	// Round-trip through JSON to match real transport behavior.
	data, err := json.Marshal(result)
	if err != nil {
		return mcp.Response{}, fmt.Errorf("mock: marshal: %w", err)
	}
	var unmarshalled any
	if err := json.Unmarshal(data, &unmarshalled); err != nil {
		return mcp.Response{}, fmt.Errorf("mock: unmarshal: %w", err)
	}
	return mcp.Response{JSONRPC: "2.0", ID: req.ID, Result: unmarshalled}, nil
}

func (m *mockTransport) Notify(_ mcp.Notification) error { return nil }
func (m *mockTransport) Close() error                    { return nil }

// setupMockProvider creates a Registry-backed Provider with a mock server registered.
func setupMockProvider(t *testing.T, serverName string) mcp.Provider {
	t.Helper()
	reg := mcp.NewRegistry()
	transport := newMockTransport()
	if err := reg.AddServer(context.Background(), serverName, transport); err != nil {
		t.Fatalf("failed to add mock server: %v", err)
	}
	return mcp.NewRegistryProvider(reg, mcp.NewAuthState())
}

func TestListMcpResources_NilProvider(t *testing.T) {
	_, err := ExecuteListMcpResources(context.Background(), map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("expected 'not available' error, got: %v", err)
	}
}

func TestListMcpResources_ServerNotFound(t *testing.T) {
	provider := mcp.NewRegistryProvider(mcp.NewRegistry(), mcp.NewAuthState())
	result, err := ExecuteListMcpResources(context.Background(), map[string]any{
		"server": "nonexistent",
	}, provider)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["error"] == nil {
		t.Error("expected error field in response")
	}
	if parsed["server"] != "nonexistent" {
		t.Errorf("expected server 'nonexistent', got %v", parsed["server"])
	}
}

func TestReadMcpResource_NilProvider(t *testing.T) {
	_, err := ExecuteReadMcpResource(context.Background(), map[string]any{"uri": "test://resource"}, nil)
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestReadMcpResource_MissingUri(t *testing.T) {
	provider := mcp.NewRegistryProvider(mcp.NewRegistry(), mcp.NewAuthState())
	_, err := ExecuteReadMcpResource(context.Background(), map[string]any{}, provider)
	if err == nil {
		t.Fatal("expected error for missing uri")
	}
	if !strings.Contains(err.Error(), "uri") {
		t.Errorf("expected error about 'uri', got: %v", err)
	}
}

func TestReadMcpResource_ServerNotFound(t *testing.T) {
	provider := mcp.NewRegistryProvider(mcp.NewRegistry(), mcp.NewAuthState())
	result, err := ExecuteReadMcpResource(context.Background(), map[string]any{
		"uri":    "test://resource",
		"server": "nonexistent",
	}, provider)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["error"] == nil {
		t.Error("expected error field in response")
	}
}

func TestMcpAuth_NilProvider(t *testing.T) {
	_, err := ExecuteMcpAuth(context.Background(), map[string]any{"server": "test"}, nil)
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestMcpAuth_MissingServer(t *testing.T) {
	provider := mcp.NewRegistryProvider(mcp.NewRegistry(), mcp.NewAuthState())
	_, err := ExecuteMcpAuth(context.Background(), map[string]any{}, provider)
	if err == nil {
		t.Fatal("expected error for missing server")
	}
	if !strings.Contains(err.Error(), "server") {
		t.Errorf("expected error about 'server', got: %v", err)
	}
}

func TestMcpAuth_ServerNotRegistered(t *testing.T) {
	provider := mcp.NewRegistryProvider(mcp.NewRegistry(), mcp.NewAuthState())

	result, err := ExecuteMcpAuth(context.Background(), map[string]any{"server": "unknown"}, provider)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["status"] != "disconnected" {
		t.Errorf("expected status 'disconnected', got %v", parsed["status"])
	}
	if parsed["message"] == nil {
		t.Error("expected message for unregistered server")
	}
}

func TestReadMcpResource_Success(t *testing.T) {
	provider := setupMockProvider(t, "test-server")

	result, err := ExecuteReadMcpResource(context.Background(), map[string]any{
		"uri":    "test://doc",
		"server": "test-server",
	}, provider)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	// Response shape: {server, uri, name, description, mime_type, content}.
	expectedKeys := []string{"server", "uri", "name", "description", "mime_type", "content"}
	if len(parsed) != len(expectedKeys) {
		t.Errorf("expected %d keys, got %d: %v", len(expectedKeys), len(parsed), parsed)
	}
	for _, key := range expectedKeys {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing key %q in response", key)
		}
	}

	if parsed["server"] != "test-server" {
		t.Errorf("expected server 'test-server', got %v", parsed["server"])
	}
	if parsed["uri"] != "test://doc" {
		t.Errorf("expected uri 'test://doc', got %v", parsed["uri"])
	}
	if parsed["name"] != "doc" {
		t.Errorf("expected name 'doc', got %v", parsed["name"])
	}
	if parsed["content"] != "Hello, world!" {
		t.Errorf("expected content 'Hello, world!', got %v", parsed["content"])
	}
}

func TestListMcpResources_Success(t *testing.T) {
	provider := setupMockProvider(t, "test-server")

	result, err := ExecuteListMcpResources(context.Background(), map[string]any{
		"server": "test-server",
	}, provider)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}

	if parsed["server"] != "test-server" {
		t.Errorf("expected server 'test-server', got %v", parsed["server"])
	}

	resources, ok := parsed["resources"].([]any)
	if !ok {
		t.Fatalf("expected resources to be an array, got %T", parsed["resources"])
	}

	count, ok := parsed["count"].(float64)
	if !ok {
		t.Fatalf("expected count to be a number, got %T", parsed["count"])
	}

	if int(count) != len(resources) {
		t.Errorf("expected count=%d to match resources length=%d", int(count), len(resources))
	}

	if len(resources) != 2 {
		t.Errorf("expected 2 resources, got %d", len(resources))
	}

	if _, ok := parsed["error"]; ok {
		t.Error("response should not contain 'error' key on success")
	}
}
