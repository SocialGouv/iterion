package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// WebSocketTransport communicates with a remote MCP server over WebSocket.
type WebSocketTransport struct {
	url     string
	headers http.Header
	conn    *websocket.Conn
	mu      sync.Mutex
	closed  bool
}

// NewWebSocketTransport creates a WebSocket transport. It dials immediately.
func NewWebSocketTransport(url string, headers map[string]string) (*WebSocketTransport, error) {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}

	dialer := websocket.DefaultDialer
	conn, _, err := dialer.Dial(url, h)
	if err != nil {
		return nil, fmt.Errorf("mcp websocket: dial %s: %w", url, err)
	}

	return &WebSocketTransport{
		url:     url,
		headers: h,
		conn:    conn,
	}, nil
}

// Send writes a JSON-RPC request and reads the response.
func (t *WebSocketTransport) Send(_ context.Context, req Request) (Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return Response{}, fmt.Errorf("mcp websocket: transport is closed")
	}

	if err := t.conn.WriteJSON(req); err != nil {
		return Response{}, fmt.Errorf("mcp websocket: write: %w", err)
	}

	_, msg, err := t.conn.ReadMessage()
	if err != nil {
		return Response{}, fmt.Errorf("mcp websocket: read: %w", err)
	}

	var resp Response
	if err := json.Unmarshal(msg, &resp); err != nil {
		return Response{}, fmt.Errorf("mcp websocket: unmarshal response: %w", err)
	}

	return resp, nil
}

// Notify sends a notification (no response expected).
func (t *WebSocketTransport) Notify(n Notification) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return fmt.Errorf("mcp websocket: transport is closed")
	}

	return t.conn.WriteJSON(n)
}

// Close closes the WebSocket connection. Idempotent.
func (t *WebSocketTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true

	// Send close message.
	_ = t.conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
	)
	return t.conn.Close()
}

// websocketURL normalizes the URL scheme for WebSocket connections.
func websocketURL(rawURL string) string {
	if strings.HasPrefix(rawURL, "http://") {
		return "ws://" + rawURL[7:]
	}
	if strings.HasPrefix(rawURL, "https://") {
		return "wss://" + rawURL[8:]
	}
	return rawURL
}
