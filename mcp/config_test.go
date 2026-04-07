package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/tool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
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
    },
    "sentry": {
      "type": "http",
      "url": "https://sentry.example.com/mcp"
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
			Servers:         []string{"sentry"},
			Disable:         []string{"falcon"},
		},
		Nodes: map[string]ir.Node{
			"implement": &ir.AgentNode{
				BaseNode: ir.BaseNode{ID: "implement"},
				MCP: &ir.MCPConfig{
					Servers: []string{"github"},
					Disable: []string{"sentry"},
				},
			},
			"act": &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "act"},
			},
		},
	}

	if err := PrepareWorkflow(wf, projectDir); err != nil {
		t.Fatalf("PrepareWorkflow: %v", err)
	}

	if got := wf.ResolvedMCPServers["github"].URL; got != "https://override.example.com/mcp" {
		t.Fatalf("expected explicit override, got %q", got)
	}
	assertStringSliceEq(t, wf.ActiveMCPServers, []string{"github", "sentry"})
	assertStringSliceEq(t, wf.Nodes["implement"].(*ir.AgentNode).ActiveMCPServers, []string{"github"})
	// ToolNode does not have ActiveMCPServers — MCP servers are only resolved for Agent/Judge nodes.
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
		MCPServers: map[string]*ir.MCPServer{
			"sentry": {
				Name:      "sentry",
				Transport: ir.MCPTransportHTTP,
				URL:       "https://sentry.example.com/mcp",
			},
		},
		MCP: &ir.MCPConfig{
			Servers: []string{"sentry"},
		},
		Nodes: map[string]ir.Node{
			"implement": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "implement"}},
		},
	}

	if err := PrepareWorkflow(wf, projectDir); err != nil {
		t.Fatalf("PrepareWorkflow: %v", err)
	}

	if _, ok := wf.ResolvedMCPServers["github"]; ok {
		t.Fatal("project .mcp.json should be ignored when ITERION_MCP_AUTOLOAD=false")
	}
	assertStringSliceEq(t, wf.ActiveMCPServers, []string{"sentry"})
}

func TestManagerHTTPDiscoveryAndCache(t *testing.T) {
	var callCalls atomic.Int32

	// Create a real MCP server using the go-sdk.
	mcpServer := gomcp.NewServer(&gomcp.Implementation{
		Name:    "test-server",
		Version: "v0.0.1",
	}, nil)

	gomcp.AddTool(mcpServer, &gomcp.Tool{
		Name:        "create_issue",
		Description: "Create a GitHub issue",
		InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input any) (*gomcp.CallToolResult, any, error) {
		callCalls.Add(1)
		return &gomcp.CallToolResult{
			Content: []gomcp.Content{
				&gomcp.TextContent{Text: "created"},
			},
		}, nil, nil
	})

	handler := gomcp.NewStreamableHTTPHandler(func(r *http.Request) *gomcp.Server {
		return mcpServer
	}, &gomcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})

	server := httptest.NewServer(handler)
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

// runStdioHelperProcess runs a real MCP server using the go-sdk on
// stdin/stdout. This is used by TestManagerStdioDiscoveryAndCall.
func runStdioHelperProcess() {
	server := gomcp.NewServer(&gomcp.Implementation{
		Name:    "echo-server",
		Version: "v0.0.1",
	}, nil)

	type echoInput struct {
		Message string `json:"message"`
	}

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "echo",
		Description: "Echo input.message",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, input echoInput) (*gomcp.CallToolResult, any, error) {
		return &gomcp.CallToolResult{
			Content: []gomcp.Content{
				&gomcp.TextContent{Text: input.Message},
			},
		}, nil, nil
	})

	if err := server.Run(context.Background(), &gomcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
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
