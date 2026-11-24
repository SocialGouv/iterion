package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func testServer() *McpSdkServer {
	return NewMcpSdkServer(McpServerSpec{
		ServerName:    "test",
		ServerVersion: "9.9.9",
		Tools:         nil,
		ToolHandler: func(name string, args json.RawMessage) (string, error) {
			return fmt.Sprintf("called %s with %s", name, string(args)), nil
		},
	})
}

func TestDispatchInitializeReturnsServerInfo(t *testing.T) {
	t.Parallel()
	s := testServer()
	resp := s.Dispatch("initialize", 1, nil)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	data, _ := json.Marshal(resp.Result)
	var result map[string]any
	json.Unmarshal(data, &result)

	if result["protocolVersion"] != MCPServerProtocolVersion {
		t.Errorf("protocolVersion = %v, want %v", result["protocolVersion"], MCPServerProtocolVersion)
	}
	si := result["serverInfo"].(map[string]any)
	if si["name"] != "test" {
		t.Errorf("serverInfo.name = %v, want test", si["name"])
	}
	if si["version"] != "9.9.9" {
		t.Errorf("serverInfo.version = %v, want 9.9.9", si["version"])
	}
}

func TestDispatchToolsListReturnsRegisteredTools(t *testing.T) {
	t.Parallel()
	desc := "Echo tool"
	schema := json.RawMessage(`{"type":"object"}`)
	s := NewMcpSdkServer(McpServerSpec{
		ServerName:    "test",
		ServerVersion: "0.0.0",
		Tools: []sdkTool{
			{Name: "echo", Description: &desc, InputSchema: &schema},
		},
		ToolHandler: func(name string, args json.RawMessage) (string, error) {
			return "", nil
		},
	})

	resp := s.Dispatch("tools/list", 2, nil)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	data, _ := json.Marshal(resp.Result)
	var result map[string]any
	json.Unmarshal(data, &result)

	tools := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "echo" {
		t.Errorf("tools[0].name = %v, want echo", tool["name"])
	}
}

func TestDispatchToolsCallWrapsHandlerOutput(t *testing.T) {
	t.Parallel()
	s := testServer()
	params := json.RawMessage(`{"name":"echo","arguments":{"text":"hi"}}`)
	resp := s.Dispatch("tools/call", 3, params)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	data, _ := json.Marshal(resp.Result)
	var result map[string]any
	json.Unmarshal(data, &result)

	isError := result["isError"].(bool)
	if isError {
		t.Errorf("isError = true, want false")
	}

	content := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("len(content) = %d, want 1", len(content))
	}
	item := content[0].(map[string]any)
	if item["type"] != "text" {
		t.Errorf("content[0].type = %v, want text", item["type"])
	}
	text := item["text"].(string)
	if !strings.HasPrefix(text, "called echo") {
		t.Errorf("content[0].text = %q, want prefix 'called echo'", text)
	}
}

func TestDispatchToolsCallSurfacesHandlerError(t *testing.T) {
	t.Parallel()
	s := NewMcpSdkServer(McpServerSpec{
		ServerName:    "test",
		ServerVersion: "0.0.0",
		ToolHandler: func(name string, args json.RawMessage) (string, error) {
			return "", fmt.Errorf("boom")
		},
	})

	params := json.RawMessage(`{"name":"broken"}`)
	resp := s.Dispatch("tools/call", 4, params)

	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", resp.Error)
	}

	data, _ := json.Marshal(resp.Result)
	var result map[string]any
	json.Unmarshal(data, &result)

	isError := result["isError"].(bool)
	if !isError {
		t.Errorf("isError = false, want true")
	}
	content := result["content"].([]any)
	item := content[0].(map[string]any)
	if item["text"] != "boom" {
		t.Errorf("content[0].text = %v, want boom", item["text"])
	}
}

func TestDispatchUnknownMethodReturnsMethodNotFound(t *testing.T) {
	t.Parallel()
	s := testServer()
	resp := s.Dispatch("nonsense", 5, nil)

	if resp.Error == nil {
		t.Fatal("expected error, got nil")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error.code = %d, want -32601", resp.Error.Code)
	}
}

func TestDispatchToolsCallMissingParams(t *testing.T) {
	t.Parallel()
	s := testServer()
	resp := s.Dispatch("tools/call", 6, nil)

	if resp.Error == nil {
		t.Fatal("expected error, got nil")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error.code = %d, want -32602", resp.Error.Code)
	}
}

func TestDispatchToolsCallInvalidParams(t *testing.T) {
	t.Parallel()
	s := testServer()
	params := json.RawMessage(`"not an object"`)
	resp := s.Dispatch("tools/call", 7, params)

	if resp.Error == nil {
		t.Fatal("expected error, got nil")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error.code = %d, want -32602", resp.Error.Code)
	}
}

func TestLSPFrameRoundTrip(t *testing.T) {
	t.Parallel()

	// Build a request frame
	reqBody := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	frame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(reqBody), reqBody)

	// Build expected response
	var input bytes.Buffer
	input.WriteString(frame)

	var output bytes.Buffer
	s := testServer()

	err := s.Run(context.Background(), &input, &output)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// Parse the output frame
	outStr := output.String()
	if !strings.HasPrefix(outStr, "Content-Length:") {
		t.Fatalf("expected Content-Length header, got: %q", outStr)
	}

	// Find the body
	idx := strings.Index(outStr, "\r\n\r\n")
	if idx < 0 {
		t.Fatal("no header/body separator found")
	}
	body := outStr[idx+4:]

	var resp sdkResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
}

func TestLSPFrameParseError(t *testing.T) {
	t.Parallel()

	// Send invalid JSON
	badBody := `{not valid json}`
	frame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(badBody), badBody)

	var input bytes.Buffer
	input.WriteString(frame)

	var output bytes.Buffer
	s := testServer()

	err := s.Run(context.Background(), &input, &output)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// Should get a -32700 parse error response
	outStr := output.String()
	idx := strings.Index(outStr, "\r\n\r\n")
	body := outStr[idx+4:]

	var resp sdkResponse
	json.Unmarshal([]byte(body), &resp)
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != -32700 {
		t.Errorf("error.code = %d, want -32700", resp.Error.Code)
	}
}

func TestNotificationIsIgnored(t *testing.T) {
	t.Parallel()

	// A notification has no "id" field
	notifBody := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	frame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(notifBody), notifBody)

	var input bytes.Buffer
	input.WriteString(frame)

	var output bytes.Buffer
	s := testServer()

	err := s.Run(context.Background(), &input, &output)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// No response should be written for a notification
	if output.Len() != 0 {
		t.Errorf("expected no output for notification, got %d bytes", output.Len())
	}
}

func TestResponseIDMatchesRequestID(t *testing.T) {
	t.Parallel()
	s := testServer()

	// Integer ID
	resp := s.Dispatch("initialize", 42, nil)
	if resp.ID != 42 {
		t.Errorf("response ID = %v, want 42", resp.ID)
	}

	// String ID
	resp = s.Dispatch("initialize", "abc-123", nil)
	if resp.ID != "abc-123" {
		t.Errorf("response ID = %v, want abc-123", resp.ID)
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Use a reader that blocks (never returns data)
	pr, _ := newBlockingPipe()
	var output bytes.Buffer

	s := testServer()
	err := s.Run(ctx, pr, &output)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// newBlockingPipe returns a reader that will block until closed.
func newBlockingPipe() (*blockingReader, chan struct{}) {
	ch := make(chan struct{})
	return &blockingReader{ch: ch}, ch
}

type blockingReader struct {
	ch chan struct{}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	<-r.ch
	return 0, fmt.Errorf("closed")
}
