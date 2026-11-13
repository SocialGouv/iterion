package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockServer reads JSON-RPC requests from r and writes responses to w.
func mockServer(t *testing.T, r *bufio.Reader, w io.Writer, handler func(req jsonRPCRequest) interface{}) {
	t.Helper()
	for {
		frame, err := readLSPFrame(r)
		if err != nil {
			return
		}
		var req jsonRPCRequest
		if err := json.Unmarshal(frame, &req); err != nil {
			return
		}
		// Notifications have no ID; don't respond.
		if req.ID == nil {
			continue
		}
		result := handler(req)
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      *req.ID,
			"result":  result,
		}
		payload, _ := json.Marshal(resp)
		if err := writeLSPFrame(w, payload); err != nil {
			return
		}
	}
}

func TestLspTransportRequestResponse(t *testing.T) {
	t.Parallel()

	// Create pipes to simulate stdin/stdout of a server process.
	// Transport writes to clientToServer, server reads from it.
	// Server writes to serverToClient, transport reads from it.
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	transport := &LspTransport{
		stdin:   clientToServerW,
		stdout:  bufio.NewReader(serverToClientR),
		pending: make(map[int64]chan json.RawMessage),
		done:    make(chan struct{}),
	}

	// Start mock server.
	serverReader := bufio.NewReader(clientToServerR)
	go mockServer(t, serverReader, serverToClientW, func(req jsonRPCRequest) interface{} {
		switch req.Method {
		case "textDocument/hover":
			return map[string]interface{}{
				"contents": map[string]string{
					"kind":  "markdown",
					"value": "func Foo()",
				},
			}
		default:
			return map[string]interface{}{"ok": true}
		}
	})

	// Start reader goroutine.
	go transport.readLoop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Send a request and verify response.
	result, err := transport.Request(ctx, "textDocument/hover", map[string]interface{}{
		"textDocument": map[string]string{"uri": "file:///test.go"},
		"position":     map[string]int{"line": 1, "character": 0},
	})
	if err != nil {
		t.Fatalf("Request() error: %v", err)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("Unmarshal result: %v", err)
	}
	contents, ok := data["contents"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected contents map, got %T", data["contents"])
	}
	if contents["value"] != "func Foo()" {
		t.Errorf("contents.value = %v, want %q", contents["value"], "func Foo()")
	}

	// Send multiple requests concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := transport.Request(ctx, "textDocument/hover", nil)
			if err != nil {
				t.Errorf("concurrent Request() error: %v", err)
				return
			}
			if len(res) == 0 {
				t.Error("concurrent Request() returned empty result")
			}
		}()
	}
	wg.Wait()

	// Clean up.
	clientToServerW.Close()
	serverToClientW.Close()
}

func TestLspTransportShutdown(t *testing.T) {
	t.Parallel()

	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	transport := &LspTransport{
		stdin:   clientToServerW,
		stdout:  bufio.NewReader(serverToClientR),
		pending: make(map[int64]chan json.RawMessage),
		done:    make(chan struct{}),
		cancel:  func() {},
	}

	// Mock server that handles shutdown.
	serverReader := bufio.NewReader(clientToServerR)
	go mockServer(t, serverReader, serverToClientW, func(req jsonRPCRequest) interface{} {
		if req.Method == "shutdown" {
			return nil
		}
		return map[string]interface{}{"ok": true}
	})

	go transport.readLoop()

	ctx := context.Background()

	// Shutdown should succeed since the server responds to the shutdown request.
	// We can't easily test process exit without a real process, but we verify
	// the request/response flow works.
	// Use a short timeout since we won't have a real process to wait for.
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err := transport.Request(shutdownCtx, "shutdown", nil)
	if err != nil {
		t.Fatalf("shutdown request error: %v", err)
	}

	// Clean up.
	clientToServerW.Close()
	serverToClientW.Close()
}

func TestLspTransportTimeout(t *testing.T) {
	t.Parallel()

	// Create a transport where the server reads requests but never responds.
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	// Drain the client-to-server pipe so writes don't block.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := clientToServerR.Read(buf); err != nil {
				return
			}
		}
	}()

	transport := &LspTransport{
		stdin:   clientToServerW,
		stdout:  bufio.NewReader(serverToClientR),
		pending: make(map[int64]chan json.RawMessage),
		done:    make(chan struct{}),
	}

	go transport.readLoop()

	// Use a very short timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := transport.Request(ctx, "textDocument/hover", nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if ctx.Err() != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", ctx.Err())
	}

	// Verify pending map is cleaned up.
	transport.pendMu.Lock()
	pendingCount := len(transport.pending)
	transport.pendMu.Unlock()
	if pendingCount != 0 {
		t.Errorf("pending count = %d, want 0", pendingCount)
	}

	// Close pipes to unblock goroutines.
	clientToServerW.Close()
	clientToServerR.Close()
	serverToClientW.Close()
}

func TestLspTransportDoneOnEOF(t *testing.T) {
	t.Parallel()

	serverToClientR, serverToClientW := io.Pipe()

	transport := &LspTransport{
		stdout:  bufio.NewReader(serverToClientR),
		pending: make(map[int64]chan json.RawMessage),
		done:    make(chan struct{}),
	}

	go transport.readLoop()

	// Close server side -> reader should detect EOF and close done.
	serverToClientW.Close()

	select {
	case <-transport.Done():
		// Expected.
	case <-time.After(2 * time.Second):
		t.Fatal("done channel not closed after EOF")
	}
}

func TestReadLSPFrameCleanEOF(t *testing.T) {
	t.Parallel()
	// Empty reader -> clean EOF -> (nil, nil)
	r := bufio.NewReader(strings.NewReader(""))
	frame, err := readLSPFrame(r)
	if frame != nil || err != nil {
		t.Errorf("clean EOF: got (%v, %v), want (nil, nil)", frame, err)
	}
}

func TestReadLSPFrameMidFrameEOF(t *testing.T) {
	t.Parallel()
	// Partial header then EOF -> should error
	r := bufio.NewReader(strings.NewReader("Content-Length: 10\r\n\r\n"))
	// The body is missing -> io.ErrUnexpectedEOF
	_, err := readLSPFrame(r)
	if err == nil {
		t.Error("expected error for mid-frame EOF")
	}
}
