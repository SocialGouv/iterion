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

// SSETransport communicates with a remote MCP server over HTTP.
// Requests are sent as JSON-RPC POSTs to /message; responses are returned directly.
// If an authHeader is provided (e.g. "Bearer <token>"), it is sent on every request.
type SSETransport struct {
	baseURL    string
	authHeader string
	httpClient *http.Client
	mu         sync.Mutex
}

// NewSSETransport creates an SSE transport for the given server URL.
// authHeader may be empty or a full "Scheme value" string (e.g. "Bearer mytoken").
func NewSSETransport(baseURL string, authHeader string) *SSETransport {
	return &SSETransport{
		baseURL:    strings.TrimRight(baseURL, "/"),
		authHeader: authHeader,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Send POSTs the JSON-RPC request to the server's /message endpoint and returns the response.
func (t *SSETransport) Send(ctx context.Context, req Request) (Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("mcp sse: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/message", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("mcp sse: build request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if t.authHeader != "" {
		httpReq.Header.Set("Authorization", t.authHeader)
	}

	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("mcp sse: http post: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		raw, _ := io.ReadAll(httpResp.Body)
		return Response{}, fmt.Errorf("mcp sse: server returned %d: %s", httpResp.StatusCode, string(raw))
	}

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("mcp sse: read response body: %w", err)
	}

	var resp Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return Response{}, fmt.Errorf("mcp sse: unmarshal response: %w", err)
	}

	return resp, nil
}

// Notify sends a notification to the server (no response expected).
func (t *SSETransport) Notify(n Notification) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	body, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("mcp sse: marshal notification: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, t.baseURL+"/message", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mcp sse: build notification request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if t.authHeader != "" {
		httpReq.Header.Set("Authorization", t.authHeader)
	}

	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("mcp sse: http post notification: %w", err)
	}
	httpResp.Body.Close()

	return nil
}

// Close is a no-op for SSE transport (no persistent connection to close).
func (t *SSETransport) Close() error {
	return nil
}
