package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeMCPServer speaks just enough MCP JSON-RPC to stand in for iterion's
// board endpoint: initialize, tools/list, tools/call.
type fakeMCPServer struct {
	mu      sync.Mutex
	calls   []string
	gotAuth string
}

func (f *fakeMCPServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		f.mu.Lock()
		f.calls = append(f.calls, req.Method)
		if v := r.Header.Get("X-Iterion-Run"); v != "" {
			f.gotAuth = v
		}
		f.mu.Unlock()

		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "fake-board", "version": "1.0.0"},
			}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []map[string]any{{
				"name":        "mcp__iterion_board__create",
				"description": "Create a card",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"title": map[string]any{"type": "string"}},
				},
			}}}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			resp["result"] = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "created " + argString(p.Arguments, "title")}},
			}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "unknown method"}
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// argString reads a string argument, or "?" when it is absent — so a wrong
// argument shape shows up in the assertion rather than as a panic.
func argString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return "?"
}

func (f *fakeMCPServer) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeMCPServer) auth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotAuth
}

// The board is offered only when the run can actually use it. Registering it
// otherwise hands the agent tools that fail on every call, which costs turns.
func TestPiMCPServers(t *testing.T) {
	full := Task{
		Capabilities:      []string{"board.create"},
		BoardHTTPEndpoint: "http://host/api/v1/mcp/board",
		BoardRunToken:     "tok-123",
	}
	t.Run("board offered when capabilities and endpoint are wired", func(t *testing.T) {
		servers := piMCPServers(full)
		if len(servers) != 1 || servers[0].Name != "iterion_board" {
			t.Fatalf("servers = %+v, want the board", servers)
		}
		if servers[0].Headers["X-Iterion-Run"] != "tok-123" {
			t.Errorf("run token missing from headers: %+v", servers[0].Headers)
		}
	})

	for name, task := range map[string]Task{
		"no capabilities": {BoardHTTPEndpoint: "http://h", BoardRunToken: "t"},
		"no endpoint":     {Capabilities: []string{"board.create"}, BoardRunToken: "t"},
		"no token":        {Capabilities: []string{"board.create"}, BoardHTTPEndpoint: "http://h"},
	} {
		t.Run("board withheld: "+name, func(t *testing.T) {
			if got := piMCPServers(task); len(got) != 0 {
				t.Errorf("servers = %+v, want none — the tools would fail on every call", got)
			}
		})
	}

	// The token must ride inside the server descriptor, never as its own
	// variable, so a generic environment dump cannot log it.
	t.Run("token appears only inside the server descriptor", func(t *testing.T) {
		env := piExtensionEnv(full)
		for k, v := range env {
			if k != "ITERION_PI_MCP_SERVERS" && v == "tok-123" {
				t.Errorf("run token leaked into %s", k)
			}
		}
		if !strings.Contains(env["ITERION_PI_MCP_SERVERS"], "tok-123") {
			t.Error("run token missing from the server descriptor")
		}
	})
}

// TestPiRPCLiveMCPBridge is the end-to-end proof: a real pi session, the real
// extension, a real HTTP MCP server — and the model successfully calling a
// tool that pi itself has no client for.
func TestPiRPCLiveMCPBridge(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	fake := &fakeMCPServer{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")
	t.Setenv("ITERION_PI_MOCK_TOOL", "mcp__iterion_board__create")
	t.Setenv("ITERION_PI_MOCK_TOOL_ARGS", `{"title":"a new card"}`)
	t.Setenv("ITERION_PI_MOCK_TEXT", "card created")

	dir := t.TempDir()
	var toolOut []string
	var mu sync.Mutex
	task := Task{
		NodeID: "board", WorkDir: dir, BaseDir: dir, StoreDir: dir,
		UserPrompt: "create a card", Model: "mock/scripted",
		Capabilities:      []string{"board.create"},
		BoardHTTPEndpoint: srv.URL,
		BoardRunToken:     "run-token-xyz",
		Hooks: TaskHooks{OnToolCalled: func(name, id string, isErr bool, out string) {
			mu.Lock()
			toolOut = append(toolOut, name+"|"+out)
			mu.Unlock()
		}},
	}

	rpc := &PiRPCBackend{Command: bin, Logger: testLogger(), ExtraArgs: []string{"-e", ext}}
	if _, err := rpc.Execute(context.Background(), task); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	methods := fake.methods()
	if !slices.Contains(methods, "initialize") || !slices.Contains(methods, "tools/list") {
		t.Fatalf("MCP handshake never happened; server saw %v", methods)
	}
	if !slices.Contains(methods, "tools/call") {
		t.Fatalf("the bridged tool was never invoked; server saw %v", methods)
	}
	if fake.auth() != "run-token-xyz" {
		t.Errorf("X-Iterion-Run = %q, want the run token — the server would reject this", fake.auth())
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(toolOut, " ")
	if !strings.Contains(joined, "created a new card") {
		t.Errorf("the board tool's result never reached the model: %q", joined)
	}
}
