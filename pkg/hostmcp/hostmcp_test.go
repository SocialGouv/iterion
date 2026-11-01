package hostmcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestRegistry_RegisterLookupRevoke(t *testing.T) {
	reg := NewRegistry()
	if _, ok := reg.Lookup("tok"); ok {
		t.Fatal("unknown token must not resolve")
	}
	err := reg.Register("tok", []string{"board.read", "board.create"}, "issue-1", "team-1", store.RunTrustDefault, true, "/work")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	g, ok := reg.Lookup("tok")
	if !ok {
		t.Fatal("registered token must resolve")
	}
	if !g.Capabilities["board.read"] || !g.Capabilities["board.create"] {
		t.Fatalf("grant capabilities = %v, want board.read+board.create", g.Capabilities)
	}
	if g.TenantID != "team-1" || g.SourceIssueID != "issue-1" || g.WorkDir != "/work" || !g.TrustStated {
		t.Fatalf("grant = %+v — tenant/source/workdir/trust must ride the grant", g)
	}
	reg.Revoke("tok")
	if _, ok := reg.Lookup("tok"); ok {
		t.Fatal("a revoked token must not resolve")
	}
}

func TestRegistry_ExpiresAfterTTL(t *testing.T) {
	reg := NewRegistry()
	now := time.Now()
	reg.now = func() time.Time { return now }
	if err := reg.Register("tok", []string{"board.read"}, "", "", store.RunTrustDefault, true, ""); err != nil {
		t.Fatal(err)
	}
	reg.now = func() time.Time { return now.Add(DefaultGrantTTL + time.Minute) }
	if _, ok := reg.Lookup("tok"); ok {
		t.Fatal("an expired token must not resolve")
	}
}

func TestRegistry_RegisterFullReturnsError(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < MaxTokens; i++ {
		if err := reg.Register(fmt.Sprintf("tok-%d", i), []string{"board.read"}, "", "", store.RunTrustDefault, true, ""); err != nil {
			t.Fatalf("Register %d within cap: %v", i, err)
		}
	}
	if err := reg.Register("one-too-many", []string{"board.read"}, "", "", store.RunTrustDefault, true, ""); err == nil {
		t.Fatal("Register beyond the cap should error — the token would 401 on every call")
	}
	if _, ok := reg.Lookup("one-too-many"); ok {
		t.Error("refused token must not be registered")
	}
}

func TestMintToken(t *testing.T) {
	a, b := MintToken(), MintToken()
	if a == "" || b == "" || a == b {
		t.Fatalf("minted tokens must be non-empty and unique, got %q / %q", a, b)
	}
}

// boardServer mounts the board routes against a resolver keyed by tenant and
// returns the test server plus the per-tenant stores the resolver served.
func boardServer(t *testing.T, tokens TokenStore, tenants ...string) (*httptest.Server, map[string]*native.Store) {
	t.Helper()
	stores := map[string]*native.Store{}
	for _, tenant := range tenants {
		st, err := native.NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		stores[tenant] = st
	}
	mux := http.NewServeMux()
	RegisterBoardRoutes(mux, "/api/v1/mcp/board", func(tenantID string) native.BoardStore {
		if st, ok := stores[tenantID]; ok {
			return st
		}
		return nil
	}, tokens)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, stores
}

func doBoardMCP(t *testing.T, srv *httptest.Server, token string, body any) *http.Response {
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

// The grant's tenant — pinned by the host at mint time — selects the board a
// token writes to. This is the cloud runner's security invariant (#2632): a
// token minted for tenant A must land on A's board and be unable to name
// tenant B's.
func TestBoardHTTP_DispatchesToTheGrantsTenant(t *testing.T) {
	reg := NewRegistry()
	srv, stores := boardServer(t, reg, "team-a", "team-b")
	if err := reg.Register("tok-a", []string{"board.create", "board.read"}, "", "team-a", store.RunTrustDefault, true, ""); err != nil {
		t.Fatal(err)
	}

	resp := doBoardMCP(t, srv, "tok-a", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "create_issue", "arguments": map[string]any{"title": "From sandbox"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
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
		t.Fatalf("create failed: %+v", r.Result.Content)
	}
	var iss native.Issue
	_ = json.Unmarshal([]byte(r.Result.Content[0]["text"].(string)), &iss)
	if got, _ := stores["team-a"].Get(iss.ID); got == nil {
		t.Fatal("the issue must land on the GRANT's tenant board (team-a)")
	}
	if got, _ := stores["team-b"].Get(iss.ID); got != nil {
		t.Fatal("the issue must NOT land on another tenant's board (team-b)")
	}
}

// A grant naming a tenant the resolver does not serve answers 404 — the
// runner's BoardFor refuses an empty tenant, and a token must never fall
// through to a default board.
func TestBoardHTTP_UnservedTenantIsNotFound(t *testing.T) {
	reg := NewRegistry()
	srv, _ := boardServer(t, reg, "team-a")
	if err := reg.Register("tok-x", []string{"board.read"}, "", "team-unknown", store.RunTrustDefault, true, ""); err != nil {
		t.Fatal(err)
	}
	resp := doBoardMCP(t, srv, "tok-x", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for a tenant this process does not serve", resp.StatusCode)
	}
}

func TestBoardHTTP_AuthAndCapability(t *testing.T) {
	reg := NewRegistry()
	srv, _ := boardServer(t, reg, "")

	resp := doBoardMCP(t, srv, "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token: status=%d, want 401", resp.StatusCode)
	}
	resp = doBoardMCP(t, srv, "garbage", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown token: status=%d, want 401", resp.StatusCode)
	}

	// A token scoped to board.read only must be refused a mutating tool.
	if err := reg.Register("reader", []string{"board.read"}, "", "", store.RunTrustDefault, true, ""); err != nil {
		t.Fatal(err)
	}
	resp = doBoardMCP(t, srv, "reader", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "create_issue", "arguments": map[string]any{"title": "X"}},
	})
	defer resp.Body.Close()
	var r struct {
		Error *ResponseError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Error == nil || !strings.Contains(r.Error.Message, "capability denied") {
		t.Fatalf("a read-only token calling create_issue must be capability-denied, got %+v", r.Error)
	}
}
