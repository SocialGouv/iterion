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
// Authentication has two modes:
//
//   - Static: authHeader is a literal "Scheme value" string sent on every request.
//   - Dynamic: authFunc is invoked per-request to produce a fresh header. Used
//     by the OAuth broker to refresh tokens transparently. authFunc takes
//     priority when set.
type SSETransport struct {
	baseURL    string
	authHeader string
	authFunc   func(ctx context.Context) (string, error)
	httpClient *http.Client
	mu         sync.Mutex
}

// NewSSETransport creates an SSE transport with a static Authorization
// header. authHeader may be empty or a full "Scheme value" string
// (e.g. "Bearer mytoken").
//
// For dynamic auth (OAuth refresh, etc.) prefer
// NewSSETransportWithAuthFunc, or call SetAuthFunc on the returned
// transport. AuthFunc, when set, takes priority over authHeader.
func NewSSETransport(baseURL string, authHeader string) *SSETransport {
	return &SSETransport{
		baseURL:    strings.TrimRight(baseURL, "/"),
		authHeader: authHeader,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// NewSSETransportWithAuthFunc creates an SSE transport with a dynamic
// Authorization-header producer. Pass nil authFunc to fall back to
// the static authHeader (equivalent to NewSSETransport).
//
// Bundling the AuthFunc into the constructor prevents the silent-
// failure mode where a caller forgets to call SetAuthFunc on a
// transport that needs OAuth: the call site is forced to acknowledge
// the auth strategy at construction time.
func NewSSETransportWithAuthFunc(baseURL, authHeader string, authFunc func(ctx context.Context) (string, error)) *SSETransport {
	t := NewSSETransport(baseURL, authHeader)
	if authFunc != nil {
		t.SetAuthFunc(authFunc)
	}
	return t
}

// SetAuthFunc installs a dynamic authorization-header producer. When
// non-nil, it is invoked on every Send / Notify and its return value
// is used as the Authorization header. This is the bridge MCP
// transports use with the OAuth broker:
//
//	transport.SetAuthFunc(broker.BearerHeaderFunc(serverCfg))
//
// Errors from authFunc abort the request with a wrapped mcp sse error.
func (t *SSETransport) SetAuthFunc(fn func(ctx context.Context) (string, error)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.authFunc = fn
}

// resolveAuth returns the Authorization header to use for a given
// request, or "" when no auth is configured. Caller must NOT hold t.mu
// — authFunc may make HTTP calls of its own (token refresh) which
// would deadlock if invoked under the transport mutex.
func (t *SSETransport) resolveAuth(ctx context.Context) (string, error) {
	t.mu.Lock()
	fn := t.authFunc
	header := t.authHeader
	t.mu.Unlock()

	if fn != nil {
		return fn(ctx)
	}
	return header, nil
}

// Send POSTs the JSON-RPC request to the server's /message endpoint and returns the response.
func (t *SSETransport) Send(ctx context.Context, req Request) (Response, error) {
	authHeader, err := t.resolveAuth(ctx)
	if err != nil {
		return Response{}, fmt.Errorf("mcp sse: resolve auth: %w", err)
	}

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
	if authHeader != "" {
		httpReq.Header.Set("Authorization", authHeader)
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
	authHeader, err := t.resolveAuth(context.Background())
	if err != nil {
		return fmt.Errorf("mcp sse: resolve auth: %w", err)
	}

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
	if authHeader != "" {
		httpReq.Header.Set("Authorization", authHeader)
	}

	httpResp, err := t.httpClient.Do(httpReq)
	if err != nil {
		// Note: Do returns (nil, err) on transport failure, so we
		// MUST NOT touch httpResp before this guard. Body.Close()
		// runs only on success below.
		return fmt.Errorf("mcp sse: http post notification: %w", err)
	}
	httpResp.Body.Close()

	return nil
}

// Close is a no-op for SSE transport (no persistent connection to close).
func (t *SSETransport) Close() error {
	return nil
}
