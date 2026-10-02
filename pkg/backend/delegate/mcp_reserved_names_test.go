package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestWireUserMCPPreservesInternalServers(t *testing.T) {
	for _, name := range []string{askUserMCPServerName, boardMCPServerName, runsMCPServerName, "Iterion.Anything", "__iterion", ".iterion", "", "__"} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelWarn, &logs)}
			task := Task{Sandbox: &captureSandboxCmdRun{}, AllowedTools: []string{"Read"}, Capabilities: []string{"board.read", "runs.read"}, BoardHTTPEndpoint: "https://internal.invalid/board", RunsHTTPEndpoint: "https://internal.invalid/runs", BoardRunToken: "synthetic", MCPServers: []TaskMCPServer{
				{Name: name, Transport: "http", URL: "https://untrusted.invalid/mcp"},
				{Name: "docs", Transport: "stdio", Command: "docs"},
			}}
			var extras []string
			opts := []claudesdk.Option{claudesdk.WithMCPServer(askUserMCPServerName, &claudesdk.MCPHTTPServer{URL: "https://internal.invalid/ask"})}
			opts = b.wireBoardMCP(task, opts, &extras)
			opts = b.wireRunsMCP(task, opts, &extras)
			opts = b.wireUserMCP(task, opts, &extras)
			var args []string
			opts = append(opts, claudesdk.WithCLIPath("fake-claude"), claudesdk.WithCommandBuilder(func(ctx context.Context, _ string, argv []string, _ string, _ map[string]string, _ bool) *exec.Cmd {
				args = append([]string(nil), argv...)
				return exec.CommandContext(ctx, "true")
			}))
			// The fake exits without a model result; only the actual serialized
			// spawn configuration matters, after Session.setupMcpConfig ran.
			session := claudesdk.NewSession(opts...)
			_ = session.Send(context.Background(), "capture config")
			_ = session.Close()
			i := slices.Index(args, "--mcp-config")
			if i < 0 || i+1 >= len(args) {
				t.Fatal("missing final MCP config")
			}
			var cfg struct {
				Servers map[string]struct {
					URL     string `json:"url"`
					Command string `json:"command"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal([]byte(args[i+1]), &cfg); err != nil {
				t.Fatal(err)
			}
			for server, want := range map[string]string{askUserMCPServerName: "https://internal.invalid/ask", boardMCPServerName: task.BoardHTTPEndpoint, runsMCPServerName: task.RunsHTTPEndpoint} {
				if got := cfg.Servers[server].URL; got != want {
					t.Errorf("internal server %s overwritten: %q, want %q", server, got, want)
				}
			}
			if len(cfg.Servers) != 4 || cfg.Servers["docs"].Command != "docs" {
				t.Errorf("unexpected final servers: %+v", cfg.Servers)
			}
			if slices.Contains(extras, "mcp__"+name+"__*") || !slices.Contains(extras, "mcp__docs__*") {
				t.Errorf("wildcard widening: %v", extras)
			}
			if !strings.Contains(logs.String(), "reserved") || !strings.Contains(logs.String(), "rename") {
				t.Errorf("missing actionable refusal: %s", &logs)
			}
		})
	}
}
