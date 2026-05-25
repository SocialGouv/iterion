package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
)

func newMCPBoardTestServer(t *testing.T) (*httptest.Server, *BoardMCPTokenRegistry, *native.Store) {
	t.Helper()
	store, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := NewBoardMCPTokenRegistry()
	mux := http.NewServeMux()
	RegisterBoardMCPRoutes(mux, "/api/v1/mcp/board", store, reg)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, reg, store
}

func doMCP(t *testing.T, srv *httptest.Server, token string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/mcp/board", bytes.NewReader(raw))
	if token != "" {
		req.Header.Set("X-Iterion-Run", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

func TestBoardMCP_HTTP_AuthRequired(t *testing.T) {
	srv, _, _ := newMCPBoardTestServer(t)
	resp := doMCP(t, srv, "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestBoardMCP_HTTP_UnknownToken(t *testing.T) {
	srv, _, _ := newMCPBoardTestServer(t)
	resp := doMCP(t, srv, "garbage", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestBoardMCP_HTTP_ToolsListFiltersByCaps(t *testing.T) {
	srv, reg, _ := newMCPBoardTestServer(t)
	reg.Register("tok", []string{"board.read"})
	defer reg.Revoke("tok")
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
	_ = json.NewDecoder(resp.Body).Decode(&r)

	expected := boardops.ToolsFor(boardops.NewCapabilities("board.read"))
	if len(r.Result.Tools) != len(expected) {
		t.Fatalf("expected %d board.read tools (per boardops registry), got %d", len(expected), len(r.Result.Tools))
	}
	want := make(map[string]bool, len(expected))
	for _, tool := range expected {
		want[tool.Name] = true
	}
	for _, tool := range r.Result.Tools {
		name, _ := tool["name"].(string)
		if !want[name] {
			t.Fatalf("MCP returned tool %q that the boardops registry does not classify as board.read", name)
		}
	}
}

func TestBoardMCP_HTTP_CreateAndRead(t *testing.T) {
	srv, reg, store := newMCPBoardTestServer(t)
	reg.Register("tok", []string{"board.create", "board.read"})
	defer reg.Revoke("tok")

	resp := doMCP(t, srv, "tok", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "create_issue",
			"arguments": map[string]any{"title": "From sandbox"},
		},
	})
	defer resp.Body.Close()
	var r struct {
		Result struct {
			Content []map[string]any `json:"content"`
			IsError bool             `json:"isError"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	if r.Result.IsError {
		t.Fatalf("expected success, got isError=true: %+v", r.Result.Content)
	}
	var iss native.Issue
	_ = json.Unmarshal([]byte(r.Result.Content[0]["text"].(string)), &iss)
	if iss.Title != "From sandbox" {
		t.Fatalf("title=%q", iss.Title)
	}
	// Verify the store really has it.
	got, _ := store.Get(iss.ID)
	if got == nil || got.Title != "From sandbox" {
		t.Fatalf("issue did not land in store: %+v", got)
	}
}

func TestBoardMCP_HTTP_CapabilityDenied(t *testing.T) {
	srv, reg, _ := newMCPBoardTestServer(t)
	reg.Register("tok", []string{"board.read"})
	defer reg.Revoke("tok")

	resp := doMCP(t, srv, "tok", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "create_issue",
			"arguments": map[string]any{"title": "X"},
		},
	})
	defer resp.Body.Close()
	var r struct {
		Error mcpRespError `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	if r.Error.Code != -32601 {
		t.Fatalf("expected -32601, got %+v", r.Error)
	}
	if !strings.Contains(r.Error.Message, "capability denied") {
		t.Fatalf("expected capability-denied message, got %q", r.Error.Message)
	}
}
