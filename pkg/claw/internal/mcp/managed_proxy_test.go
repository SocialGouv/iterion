package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestManagedProxyTransportRoundTrip(t *testing.T) {
	var capturedProxyID string
	var capturedContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedProxyID = r.Header.Get("X-MCP-Proxy-Id")
		capturedContentType = r.Header.Get("Content-Type")

		body, _ := io.ReadAll(r.Body)
		var req Request
		json.Unmarshal(body, &req)

		resp := Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]interface{}{"proxied": true},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	transport, err := NewManagedProxyTransport(server.URL, "test-proxy-123")
	if err != nil {
		t.Fatalf("create transport: %v", err)
	}

	resp, err := transport.Send(t.Context(), Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/list",
	})
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}

	if capturedProxyID != "test-proxy-123" {
		t.Errorf("expected X-MCP-Proxy-Id=test-proxy-123, got %q", capturedProxyID)
	}
	if capturedContentType != "application/json" {
		t.Errorf("expected Content-Type=application/json, got %q", capturedContentType)
	}
	if resp.ID != float64(1) {
		t.Errorf("expected ID=1, got %v", resp.ID)
	}
}

func TestManagedProxyTransportHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer server.Close()

	transport, err := NewManagedProxyTransport(server.URL, "test-proxy")
	if err != nil {
		t.Fatalf("create transport: %v", err)
	}

	_, err = transport.Send(t.Context(), Request{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestManagedProxyTransportNotify(t *testing.T) {
	var capturedProxyID string
	var capturedMethod string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedProxyID = r.Header.Get("X-MCP-Proxy-Id")
		body, _ := io.ReadAll(r.Body)
		var n struct {
			Method string `json:"method"`
		}
		json.Unmarshal(body, &n)
		capturedMethod = n.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport, err := NewManagedProxyTransport(server.URL, "notif-proxy-456")
	if err != nil {
		t.Fatalf("create transport: %v", err)
	}

	err = transport.Notify(Notification{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
	if err != nil {
		t.Fatalf("Notify failed: %v", err)
	}

	if capturedProxyID != "notif-proxy-456" {
		t.Errorf("expected X-MCP-Proxy-Id=notif-proxy-456, got %q", capturedProxyID)
	}
	if capturedMethod != "notifications/initialized" {
		t.Errorf("expected method 'notifications/initialized', got %q", capturedMethod)
	}
}

func TestManagedProxyTransportCloseIsNoop(t *testing.T) {
	transport, _ := NewManagedProxyTransport("http://example.com", "id")
	if err := transport.Close(); err != nil {
		t.Fatalf("close should be nil: %v", err)
	}
}
