package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ManagedProxyTransport communicates with a managed MCP proxy over HTTP.
// The proxy is identified by the X-MCP-Proxy-Id header.
type ManagedProxyTransport struct {
	url        string
	id         string
	httpClient *http.Client
	mu         sync.Mutex
}

// NewManagedProxyTransport creates a managed proxy transport.
func NewManagedProxyTransport(url string, id string) (*ManagedProxyTransport, error) {
	return &ManagedProxyTransport{
		url:        strings.TrimRight(url, "/"),
		id:         id,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Send POSTs the JSON-RPC request to the proxy and returns the response.
func (t *ManagedProxyTransport) Send(ctx context.Context, req Request) (Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("mcp managed_proxy: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("mcp managed_proxy: build request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-MCP-Proxy-Id", t.id)

	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("mcp managed_proxy: http post: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		raw, _ := io.ReadAll(httpResp.Body)
		return Response{}, fmt.Errorf("mcp managed_proxy: server returned %d: %s", httpResp.StatusCode, string(raw))
	}

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("mcp managed_proxy: read response body: %w", err)
	}

	var resp Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return Response{}, fmt.Errorf("mcp managed_proxy: unmarshal response: %w", err)
	}

	return resp, nil
}

// Notify sends a notification to the proxy (no response expected).
func (t *ManagedProxyTransport) Notify(n Notification) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	body, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("mcp managed_proxy: marshal notification: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mcp managed_proxy: build notification request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-MCP-Proxy-Id", t.id)

	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("mcp managed_proxy: http post notification: %w", err)
	}
	httpResp.Body.Close()

	return nil
}

// Close is a no-op for managed proxy transport.
func (t *ManagedProxyTransport) Close() error {
	return nil
}
