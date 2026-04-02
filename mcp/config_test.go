package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/tool"
)

func TestPrepareWorkflowProjectAutoloadAndOverrides(t *testing.T) {
	projectDir := t.TempDir()
	writeProjectMCPFile(t, projectDir, `{
  "mcpServers": {
    "github": {
      "type": "http",
      "url": "https://project.example.com/mcp"
    },
    "falcon": {
      "type": "stdio",
      "command": "falcon",
      "args": ["mcp"]
    }
  }
}`)

	autoload := true
	wf := &ir.Workflow{
		MCPServers: map[string]*ir.MCPServer{
			"github": {
				Name:      "github",
				Transport: ir.MCPTransportHTTP,
				URL:       "https://override.example.com/mcp",
			},
		},
		MCP: &ir.MCPConfig{
			AutoloadProject: &autoload,
			Servers:         []string{"codex"},
			Disable:         []string{"falcon"},
		},
		Nodes: map[string]*ir.Node{
			"implement": {
				ID:   "implement",
				Kind: ir.NodeAgent,
				MCP: &ir.MCPConfig{
					Servers: []string{"claude_code"},
					Disable: []string{"codex"},
				},
			},
			"act": {
				ID:   "act",
				Kind: ir.NodeTool,
			},
		},
	}

	if err := PrepareWorkflow(wf, projectDir); err != nil {
		t.Fatalf("PrepareWorkflow: %v", err)
	}

	if got := wf.ResolvedMCPServers["github"].URL; got != "https://override.example.com/mcp" {
		t.Fatalf("expected explicit override, got %q", got)
	}
	assertStringSliceEq(t, wf.ActiveMCPServers, []string{"github", "codex"})
	assertStringSliceEq(t, wf.Nodes["implement"].ActiveMCPServers, []string{"github", "claude_code"})
	assertStringSliceEq(t, wf.Nodes["act"].ActiveMCPServers, []string{"github", "codex"})
}

func TestPrepareWorkflowAutoloadDisabledByEnv(t *testing.T) {
	t.Setenv(EnvAutoLoad, "false")

	projectDir := t.TempDir()
	writeProjectMCPFile(t, projectDir, `{
  "mcpServers": {
    "github": {
      "type": "http",
      "url": "https://project.example.com/mcp"
    }
  }
}`)

	wf := &ir.Workflow{
		MCP: &ir.MCPConfig{
			Servers: []string{"claude_code"},
		},
		Nodes: map[string]*ir.Node{
			"implement": {ID: "implement", Kind: ir.NodeAgent},
		},
	}

	if err := PrepareWorkflow(wf, projectDir); err != nil {
		t.Fatalf("PrepareWorkflow: %v", err)
	}

	if _, ok := wf.ResolvedMCPServers["github"]; ok {
		t.Fatal("project .mcp.json should be ignored when ITERION_MCP_AUTOLOAD=false")
	}
	assertStringSliceEq(t, wf.ActiveMCPServers, []string{"claude_code"})
}

func TestPrepareWorkflowActivatesPresetsWithoutProjectFile(t *testing.T) {
	wf := &ir.Workflow{
		MCP: &ir.MCPConfig{
			Servers: []string{"claude_code", "codex"},
		},
		Nodes: map[string]*ir.Node{
			"implement": {ID: "implement", Kind: ir.NodeAgent},
		},
	}

	if err := PrepareWorkflow(wf, t.TempDir()); err != nil {
		t.Fatalf("PrepareWorkflow: %v", err)
	}

	if _, ok := wf.ResolvedMCPServers["claude_code"]; !ok {
		t.Fatal("expected claude_code preset in resolved catalog")
	}
	if _, ok := wf.ResolvedMCPServers["codex"]; !ok {
		t.Fatal("expected codex preset in resolved catalog")
	}
	assertStringSliceEq(t, wf.ActiveMCPServers, []string{"claude_code", "codex"})
}

func TestManagerHTTPDiscoveryAndCache(t *testing.T) {
	var initializeCalls atomic.Int32
	var listCalls atomic.Int32
	var callCalls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch req.Method {
		case "initialize":
			initializeCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "session-1")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"protocolVersion": DefaultProtocolVersion,
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			listCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"tools": []map[string]interface{}{
						{
							"name":        "create_issue",
							"description": "Create a GitHub issue",
							"inputSchema": map[string]interface{}{
								"type": "object",
							},
						},
					},
				},
			})
		case "tools/call":
			callCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"content": []map[string]interface{}{
						{"type": "text", "text": "created"},
					},
				},
			})
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	manager := NewManager(map[string]*ServerConfig{
		"github": {
			Name:      "github",
			Transport: TransportHTTP,
			URL:       server.URL,
		},
	})
	registry := tool.NewRegistry()

	if err := manager.EnsureServers(context.Background(), registry, []string{"github"}); err != nil {
		t.Fatalf("EnsureServers first call: %v", err)
	}
	if err := manager.EnsureServers(context.Background(), registry, []string{"github"}); err != nil {
		t.Fatalf("EnsureServers second call: %v", err)
	}

	if got := listCalls.Load(); got != 1 {
		t.Fatalf("expected one tools/list call, got %d", got)
	}
	if got := initializeCalls.Load(); got != 1 {
		t.Fatalf("expected one initialize call, got %d", got)
	}

	td, err := registry.Resolve("mcp.github.create_issue")
	if err != nil {
		t.Fatalf("Resolve registered MCP tool: %v", err)
	}
	out, err := td.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute registered MCP tool: %v", err)
	}
	if out != "created" {
		t.Fatalf("unexpected tool output %q", out)
	}
	if got := callCalls.Load(); got != 1 {
		t.Fatalf("expected one tools/call request, got %d", got)
	}
}

func TestManagerStdioDiscoveryAndCall(t *testing.T) {
	if helperProcessMode() {
		runStdioHelperProcess()
		return
	}

	manager := NewManager(map[string]*ServerConfig{
		"echo": {
			Name:      "echo",
			Transport: TransportStdio,
			Command:   os.Args[0],
			Args:      []string{"-test.run=TestManagerStdioDiscoveryAndCall", "--", "mcp-stdio-helper"},
		},
	})
	registry := tool.NewRegistry()

	if err := manager.EnsureServers(context.Background(), registry, []string{"echo"}); err != nil {
		t.Fatalf("EnsureServers: %v", err)
	}

	td, err := registry.Resolve("mcp.echo.echo")
	if err != nil {
		t.Fatalf("Resolve stdio MCP tool: %v", err)
	}
	out, err := td.Execute(context.Background(), json.RawMessage(`{"message":"hello"}`))
	if err != nil {
		t.Fatalf("Execute stdio MCP tool: %v", err)
	}
	if out != "hello" {
		t.Fatalf("unexpected stdio tool output %q", out)
	}
}

func helperProcessMode() bool {
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) && os.Args[i+1] == "mcp-stdio-helper" {
			return true
		}
	}
	return false
}

func runStdioHelperProcess() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024), maxMessageSize)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			panic(err)
		}
		switch req.Method {
		case "initialize":
			writeHelperResponse(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"protocolVersion": DefaultProtocolVersion,
				},
			})
		case "notifications/initialized":
			// Notification: no response.
		case "tools/list":
			writeHelperResponse(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"tools": []map[string]interface{}{
						{
							"name":        "echo",
							"description": "Echo input.message",
							"inputSchema": map[string]interface{}{
								"type": "object",
							},
						},
					},
				},
			})
		case "tools/call":
			var params callToolParams
			raw, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(raw, &params)
			writeHelperResponse(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"content": []map[string]interface{}{
						{"type": "text", "text": fmt.Sprint(params.Arguments["message"])},
					},
				},
			})
		default:
			writeHelperResponse(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error": map[string]interface{}{
					"code":    -32601,
					"message": "method not found",
				},
			})
		}
	}
	os.Exit(0)
}

func writeHelperResponse(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))
}

func writeProjectMCPFile(t *testing.T, dir, contents string) {
	t.Helper()
	path := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertStringSliceEq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("slice length mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slice mismatch at %d: got %v want %v", i, got, want)
		}
	}
}
