package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runops"
	"github.com/SocialGouv/iterion/pkg/store"
)

type tenantCheckingRunStore struct {
	store.RunStore
	want string
}

func (s tenantCheckingRunStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if got, ok := store.TenantFromContext(ctx); !ok || got != s.want {
		return nil, context.Canceled
	}
	return s.RunStore.LoadRun(context.Background(), id)
}

func TestRunsMCPHTTPPinsTenantAndCapability(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.CreateRun(context.Background(), "run-tenant", "demo", nil); err != nil {
		t.Fatal(err)
	}
	reg := NewBoardMCPTokenRegistry()
	if err := reg.Register("token", []string{runops.CapRunsRead}, "", "team-1"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterRunsMCPRoutes(mux, "/api/v1/mcp/runs", tenantCheckingRunStore{RunStore: base, want: "team-1"}, reg)

	id := json.RawMessage(`1`)
	body, _ := json.Marshal(mcpReq{JSONRPC: "2.0", ID: &id, Method: "tools/call", Params: json.RawMessage(`{"name":"run_get","arguments":{"run_id":"run-tenant"}}`)})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/runs", bytes.NewReader(body))
	req.Header.Set("X-Iterion-Run", "token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`\"id\":\"run-tenant\"`)) {
		t.Fatalf("unexpected MCP response: %s", rec.Body.String())
	}
}

func TestRunsMCPHTTPDoesNotListToolsWithoutCapability(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := NewBoardMCPTokenRegistry()
	if err := reg.Register("token", []string{"board.read"}, "", ""); err != nil {
		t.Fatal(err)
	}
	id := json.RawMessage(`1`)
	resp := dispatchRunsHTTP(context.Background(), mcpReq{JSONRPC: "2.0", ID: &id, Method: "tools/list"}, base, runops.Capabilities{})
	encoded, _ := json.Marshal(resp.Result)
	if string(encoded) != `{"tools":[]}` {
		t.Fatalf("tools/list = %s", encoded)
	}
}
