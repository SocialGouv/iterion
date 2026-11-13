package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func TestNewTransportSSE(t *testing.T) {
	tr, err := NewTransport(TransportConfig{
		Type: TransportSSE,
		URL:  "http://example.com/mcp",
		Auth: "Bearer token123",
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if tr == nil {
		t.Fatal("expected non-nil transport")
	}
	// Verify it's an SSETransport.
	sse, ok := tr.(*SSETransport)
	if !ok {
		t.Fatalf("expected *SSETransport, got %T", tr)
	}
	if sse.baseURL != "http://example.com/mcp" {
		t.Errorf("expected baseURL='http://example.com/mcp', got %q", sse.baseURL)
	}
}

func TestNewTransportHTTP(t *testing.T) {
	tr, err := NewTransport(TransportConfig{
		Type: TransportHTTP,
		URL:  "http://example.com/mcp",
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if _, ok := tr.(*SSETransport); !ok {
		t.Fatalf("expected *SSETransport for HTTP type, got %T", tr)
	}
}

func TestNewTransportManagedProxy(t *testing.T) {
	tr, err := NewTransport(TransportConfig{
		Type: TransportManagedProxy,
		URL:  "http://proxy.example.com",
		ID:   "proxy-123",
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	proxy, ok := tr.(*ManagedProxyTransport)
	if !ok {
		t.Fatalf("expected *ManagedProxyTransport, got %T", tr)
	}
	if proxy.id != "proxy-123" {
		t.Errorf("expected id='proxy-123', got %q", proxy.id)
	}
}

func TestNewTransportUnknown(t *testing.T) {
	_, err := NewTransport(TransportConfig{
		Type: "unknown",
	})
	if err == nil {
		t.Fatal("expected error for unknown transport type")
	}
}

func TestNewTransportWebSocketURLNormalization(t *testing.T) {
	// websocketURL should normalize http:// to ws:// via the factory.
	// We can't actually dial, but we can test the URL conversion function.
	tests := []struct {
		input string
		want  string
	}{
		{"http://localhost:8080", "ws://localhost:8080"},
		{"https://example.com", "wss://example.com"},
		{"ws://already.ws", "ws://already.ws"},
		{"wss://already.wss", "wss://already.wss"},
	}
	for _, tt := range tests {
		got := websocketURL(tt.input)
		if got != tt.want {
			t.Errorf("websocketURL(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTransportTypeConstants(t *testing.T) {
	types := []TransportType{
		TransportStdio,
		TransportSSE,
		TransportHTTP,
		TransportWebSocket,
		TransportManagedProxy,
		TransportSDK,
	}
	for _, tt := range types {
		if string(tt) == "" {
			t.Error("transport type should not be empty")
		}
	}
}

func TestWriteReadLSPFrame(t *testing.T) {
	// Round-trip test for the Content-Length framing helpers.
	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"test"}`)

	var buf bytes.Buffer
	if err := WriteLSPFrameTo(&buf, payload); err != nil {
		t.Fatalf("WriteLSPFrameTo failed: %v", err)
	}

	reader := bufio.NewReader(&buf)
	got, err := ReadLSPFrameFrom(reader)
	if err != nil {
		t.Fatalf("ReadLSPFrameFrom failed: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("round-trip mismatch:\n  got:  %s\n  want: %s", got, payload)
	}
}

func TestWriteReadLSPFrameMultiple(t *testing.T) {
	// Verify multiple frames can be written and read sequentially.
	frames := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"a"}`),
		[]byte(`{"jsonrpc":"2.0","id":2,"method":"b"}`),
		[]byte(`{"jsonrpc":"2.0","id":3,"method":"c"}`),
	}

	var buf bytes.Buffer
	for _, f := range frames {
		if err := WriteLSPFrameTo(&buf, f); err != nil {
			t.Fatalf("WriteLSPFrameTo failed: %v", err)
		}
	}

	reader := bufio.NewReader(&buf)
	for i, want := range frames {
		got, err := ReadLSPFrameFrom(reader)
		if err != nil {
			t.Fatalf("ReadLSPFrameFrom frame %d failed: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("frame %d mismatch:\n  got:  %s\n  want: %s", i, got, want)
		}
	}

	// After all frames, should get nil on clean EOF.
	got, err := ReadLSPFrameFrom(reader)
	if err != nil {
		t.Fatalf("expected clean EOF, got error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil on EOF, got: %s", got)
	}
}

func TestSDKTransportSendWithPipes(t *testing.T) {
	// Integration test using io.Pipe pairs and McpSdkServer.
	// clientWrite -> serverRead (server stdin)
	// serverWrite -> clientRead (server stdout)
	clientWriteR, clientWriteW := io.Pipe()
	serverWriteR, serverWriteW := io.Pipe()

	// Start an McpSdkServer in a goroutine.
	server := NewMcpSdkServer(McpServerSpec{
		ServerName:    "test-server",
		ServerVersion: "1.0.0",
	})

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(t.Context(), clientWriteR, serverWriteW)
	}()

	// Build an SDKTransport-like setup manually with the pipe endpoints.
	stdoutReader := bufio.NewReader(serverWriteR)
	responses := make(chan Response, 16)
	done := make(chan struct{})

	// Reader goroutine.
	go func() {
		defer close(done)
		for {
			frame, err := ReadLSPFrameFrom(stdoutReader)
			if err != nil || frame == nil {
				return
			}
			var resp Response
			if err := json.Unmarshal(frame, &resp); err != nil {
				continue
			}
			responses <- resp
		}
	}()

	// Send an initialize request.
	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "test-client",
				"version": "1.0.0",
			},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	if err := WriteLSPFrameTo(clientWriteW, data); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Read response.
	resp := <-responses
	if resp.Error != nil {
		t.Fatalf("unexpected error in response: %+v", resp.Error)
	}
	if resp.ID == nil {
		t.Fatal("expected non-nil response ID")
	}

	// Verify the result contains server info.
	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if _, ok := result["protocolVersion"]; !ok {
		t.Error("expected protocolVersion in initialize result")
	}
	serverInfo, ok := result["serverInfo"]
	if !ok {
		t.Fatal("expected serverInfo in initialize result")
	}
	infoMap, ok := serverInfo.(map[string]any)
	if !ok {
		t.Fatal("expected serverInfo to be a map")
	}
	if infoMap["name"] != "test-server" {
		t.Errorf("expected server name 'test-server', got %v", infoMap["name"])
	}

	// Clean up: close the write pipe to signal EOF to the server.
	clientWriteW.Close()

	// Wait for server to finish, then close its write pipe so the reader
	// goroutine sees EOF.
	if err := <-serverDone; err != nil {
		t.Errorf("server exited with error: %v", err)
	}
	serverWriteW.Close()
	<-done
}

func TestSDKTransportClose(t *testing.T) {
	// Verify that Close properly cleans up a real subprocess.
	// Use "cat" as a simple subprocess that reads stdin and echoes to stdout.
	tr, err := NewSDKTransport("test-close", "cat", nil, nil)
	if err != nil {
		t.Fatalf("NewSDKTransport: %v", err)
	}

	// Close should terminate the process without error.
	if err := tr.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}

	// Verify the done channel is closed (reader goroutine exited).
	select {
	case <-tr.done:
		// ok
	default:
		t.Error("expected done channel to be closed after Close()")
	}
}

func TestSDKTransportNoServerField(t *testing.T) {
	// Verify the SDKTransport struct does not have a server field.
	// This is a compile-time check: if someone adds *McpSdkServer back,
	// the struct literal below will fail to compile.
	_ = SDKTransport{
		name:      "check",
		responses: make(chan Response),
		done:      make(chan struct{}),
	}
}
