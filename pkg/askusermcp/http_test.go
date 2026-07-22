package askusermcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

func newTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(DefaultPath, token))
	t.Cleanup(srv.Close)
	return srv
}

func doMCP(t *testing.T, srv *httptest.Server, token string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+DefaultPath, bytes.NewReader(raw))
	if token != "" {
		req.Header.Set("X-Iterion-Run", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

func TestAskUserMCP_HTTP_AuthRequired(t *testing.T) {
	srv := newTestServer(t, "tok")
	resp := doMCP(t, srv, "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAskUserMCP_HTTP_UnknownToken(t *testing.T) {
	srv := newTestServer(t, "tok")
	resp := doMCP(t, srv, "garbage", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// An empty configured token must fail closed (401 for every bearer),
// never authorize-all.
func TestAskUserMCP_HTTP_EmptyConfiguredTokenFailsClosed(t *testing.T) {
	srv := newTestServer(t, "")
	resp := doMCP(t, srv, "anything", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on empty configured token, got %d", resp.StatusCode)
	}
}

// serverInfo.version is REQUIRED by the MCP spec: the claude-code client
// validates the initialize response with a Zod schema and rejects the
// whole connection when it's missing (the C082 board lesson).
func TestAskUserMCP_HTTP_InitializeServerInfoVersion(t *testing.T) {
	srv := newTestServer(t, "tok")
	resp := doMCP(t, srv, "tok", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var r struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Result.ServerInfo.Version == "" {
		t.Fatal("initialize response is missing serverInfo.version — claude-code's Zod schema rejects the connection without it (C082)")
	}
	if r.Result.ServerInfo.Name == "" {
		t.Fatal("initialize response is missing serverInfo.name")
	}
}

func TestAskUserMCP_HTTP_ToolsList(t *testing.T) {
	srv := newTestServer(t, "tok")
	resp := doMCP(t, srv, "tok", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var r struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]bool{
		AskUserToolName:               false,
		delegate.AskUserAsyncToolName: false,
		delegate.AwaitAnswersToolName: false,
	}
	for _, tool := range r.Result.Tools {
		name, _ := tool["name"].(string)
		if _, ok := want[name]; !ok {
			t.Fatalf("unexpected tool %q", name)
		}
		want[name] = true
		if tool["inputSchema"] == nil {
			t.Errorf("tool %q has no inputSchema", name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tools/list is missing %q", name)
		}
	}
}

// ask_user_async's tools/call is the REAL success path (the PreToolUse
// hook persists the interaction host-side and ALLOWS the call through):
// the canned text must tell the model to keep working, not error.
func TestAskUserMCP_HTTP_AsyncCallReturnsPostedText(t *testing.T) {
	srv := newTestServer(t, "tok")
	resp := doMCP(t, srv, "tok", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      delegate.AskUserAsyncToolName,
			"arguments": map[string]any{"question": "color?"},
		},
	})
	defer resp.Body.Close()
	var r struct {
		Result struct {
			Content []map[string]any `json:"content"`
			IsError bool             `json:"isError"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Result.IsError {
		t.Fatalf("ask_user_async tools/call must not error: %+v", r.Result.Content)
	}
	if len(r.Result.Content) != 1 || r.Result.Content[0]["text"] != delegate.AsyncQuestionPostedText {
		t.Fatalf("ask_user_async must return the canned keep-working text, got %+v", r.Result.Content)
	}
}

// ask_user is intercepted by the host-side PreToolUse hook; reaching the
// server means the hook was bypassed — the defensive fallback must be a
// loud error, never a fabricated answer.
func TestAskUserMCP_HTTP_BlockingCallIsDefensiveError(t *testing.T) {
	srv := newTestServer(t, "tok")
	resp := doMCP(t, srv, "tok", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      AskUserToolName,
			"arguments": map[string]any{"question": "may I?"},
		},
	})
	defer resp.Body.Close()
	var r struct {
		Result struct {
			Content []map[string]any `json:"content"`
			IsError bool             `json:"isError"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.Result.IsError {
		t.Fatal("un-intercepted ask_user must surface as an error result")
	}
	text, _ := r.Result.Content[0]["text"].(string)
	if !strings.Contains(text, "ESCALATION_NOT_INTERCEPTED") {
		t.Fatalf("expected the ESCALATION_NOT_INTERCEPTED marker, got %q", text)
	}
}

// Per the MCP Streamable HTTP contract, GET must answer 405 (no SSE
// stream offered) — a 404 makes the claude-code client abort the whole
// connection (the C082 board lesson).
func TestAskUserMCP_HTTP_GetIsMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, "tok")
	resp, err := http.Get(srv.URL + DefaultPath)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "POST" {
		t.Fatalf("Allow=%q, want POST", allow)
	}
}

func TestAskUserMCP_HTTP_NotificationIsNoContent(t *testing.T) {
	srv := newTestServer(t, "tok")
	// No "id" → notification: no JSON-RPC response expected.
	resp := doMCP(t, srv, "tok", map[string]any{"jsonrpc": "2.0", "method": "initialize"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for a notification, got %d", resp.StatusCode)
	}
}

func TestAskUserMCP_HTTP_ParseError(t *testing.T) {
	srv := newTestServer(t, "tok")
	req, _ := http.NewRequest("POST", srv.URL+DefaultPath, strings.NewReader("{not json"))
	req.Header.Set("X-Iterion-Run", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	var r struct {
		Error mcpRespError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Error.Code != -32700 {
		t.Fatalf("expected -32700 parse error, got %+v", r.Error)
	}
}

func TestAskUserMCP_HTTP_MethodNotFound(t *testing.T) {
	srv := newTestServer(t, "tok")
	resp := doMCP(t, srv, "tok", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"})
	defer resp.Body.Close()
	var r struct {
		Error mcpRespError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Error.Code != -32601 {
		t.Fatalf("expected -32601, got %+v", r.Error)
	}
}

func TestNewRunToken(t *testing.T) {
	a, err := NewRunToken()
	if err != nil {
		t.Fatalf("NewRunToken: %v", err)
	}
	b, err := NewRunToken()
	if err != nil {
		t.Fatalf("NewRunToken: %v", err)
	}
	if len(a) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(a))
	}
	if a == b {
		t.Fatal("two minted tokens must differ")
	}
}
