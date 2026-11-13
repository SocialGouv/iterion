package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

var testUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsEchoServer returns an httptest.Server that keeps the WebSocket alive,
// draining incoming messages until the client disconnects. If handler is
// non-nil it is called for each incoming message before draining resumes.
func wsEchoServer(handler func(conn *websocket.Conn)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if handler != nil {
			handler(conn)
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
}

func wsURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func TestWebSocketTransportRoundTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}
		defer conn.Close()

		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req Request
		json.Unmarshal(msg, &req)

		conn.WriteJSON(Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]interface{}{"echo": true},
		})
	}))
	defer server.Close()

	transport, err := NewWebSocketTransport(wsURL(server), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer transport.Close()

	resp, err := transport.Send(t.Context(), Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "test/echo",
	})
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if resp.ID != float64(1) {
		t.Errorf("expected ID=1, got %v", resp.ID)
	}
}

func TestWebSocketTransportConnectionFailure(t *testing.T) {
	_, err := NewWebSocketTransport("ws://127.0.0.1:1", nil)
	if err == nil {
		t.Fatal("expected error for bad address")
	}
}

func TestWebSocketTransportNotify(t *testing.T) {
	notifReceived := make(chan Notification, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var n Notification
		json.Unmarshal(msg, &n)
		notifReceived <- n
		conn.ReadMessage() // keep alive
	}))
	defer server.Close()

	transport, err := NewWebSocketTransport(wsURL(server), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer transport.Close()

	if err := transport.Notify(Notification{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		t.Fatalf("Notify failed: %v", err)
	}

	n := <-notifReceived
	if n.Method != "notifications/initialized" {
		t.Errorf("expected method 'notifications/initialized', got %q", n.Method)
	}
}

func TestWebSocketTransportErrorAfterClose(t *testing.T) {
	server := wsEchoServer(nil)
	defer server.Close()

	transport, err := NewWebSocketTransport(wsURL(server), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	transport.Close()

	_, err = transport.Send(t.Context(), Request{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err == nil {
		t.Fatal("expected error on Send after Close")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("expected 'closed' in error, got: %v", err)
	}

	if err := transport.Notify(Notification{JSONRPC: "2.0", Method: "test"}); err == nil {
		t.Fatal("expected error on Notify after Close")
	}
}

func TestWebSocketTransportCloseIdempotent(t *testing.T) {
	server := wsEchoServer(nil)
	defer server.Close()

	transport, err := NewWebSocketTransport(wsURL(server), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	if err := transport.Close(); err != nil {
		t.Fatalf("first close failed: %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("second close should be nil, got: %v", err)
	}
}
