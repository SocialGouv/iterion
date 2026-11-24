package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// converseMCPServer is the ambient server the fixture's plugin contributes —
// the platform's firecrawl plugin, in production.
const converseMCPServer = "webprobe"

// TestReviConverseAgentInheritsNoAmbientMCPServer proves revi-converse's
// converse_agent carries no MCP server on either backend a routing cell can
// move it to. An operator plugin contributes one ambient server (an HTTP MCP
// server serving one tool), resolved the way a run resolves it
// (mcp.PrepareWorkflow), and the shipped node is then built by the executor
// for claw — the tool definitions its loop sends — and for claude_code, whose
// real backend spawns a stand-in CLI that records the `--mcp-config` catalog
// it was handed.
//
// The same node with its `mcp:` block removed is the control: it inherits the
// server, claw builds its tool, and the claude_code catalog names it — so a
// silent fixture (a plugin that did not load, a server that did not answer)
// cannot pass for an opted-out node.
//
// The deployment lifts claude_code's --strict-mcp-config for every node
// (ITERION_CLAUDE_CODE_STRICT_MCP=0): the opted-out node keeps it on every
// spawn, so the CLI's own scopes add no server either; the control loses it.
//
// Mutation that reddens it: drop `mcp: inherit: false` from converse_agent
// in bots/revi-converse/main.bot; drop the MCPInheritDisabled arm of
// claudeSpawnBounds.
func TestReviConverseAgentInheritsNoAmbientMCPServer(t *testing.T) {
	peer := converseMCPPeer(t)
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	t.Setenv(mcp.EnvAutoLoad, "")
	t.Setenv("ITERION_CLAUDE_CODE_STRICT_MCP", "0")
	plugin := filepath.Join(home, "plugins", "webprobe")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name: webprobe\nversion: 1.0.0\ndescription: web search\nschema_version: 1\ndefault_enabled: true\n" +
		"contributes:\n  mcp_servers:\n    - name: " + converseMCPServer + "\n      transport: http\n      url: " + peer + "\n"
	if err := os.WriteFile(filepath.Join(plugin, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		shipped bool
	}{
		{"shipped node", true},
		{"control: the node without its mcp block", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			wf, node := compileReviConverseAgent(t)
			if !tc.shipped {
				node.MCP = nil
			}
			if err := mcp.PrepareWorkflow(wf, ws, ws, store.RunTrustDefault); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(wf.ActiveMCPServers, converseMCPServer) {
				t.Fatalf("the workflow's ambient set %v lacks the plugin's server — the fixture proves nothing", wf.ActiveMCPServers)
			}
			inherits := slices.Contains(node.ActiveMCPServers, converseMCPServer)
			if tc.shipped && len(node.ActiveMCPServers) != 0 {
				t.Errorf("converse_agent's active MCP servers = %v, want none", node.ActiveMCPServers)
			}
			if !tc.shipped && !inherits {
				t.Fatalf("control: the node without its mcp block did not inherit %q (active %v)", converseMCPServer, node.ActiveMCPServers)
			}

			manager := converseMCPManager(t, wf)
			clawTools := converseClawToolNames(t, wf, node, ws, manager)
			var mcpTools []string
			for _, name := range clawTools {
				if strings.HasPrefix(name, "mcp_") || strings.HasPrefix(name, "mcp.") {
					mcpTools = append(mcpTools, name)
				}
			}
			if tc.shipped && len(mcpTools) != 0 {
				t.Errorf("claw tool definitions %v carry MCP tools %v", clawTools, mcpTools)
			}
			if !tc.shipped && !slices.Contains(mcpTools, "mcp_"+converseMCPServer+"_scrape") {
				t.Fatalf("control: claw tool definitions %v lack the inherited server's tool", clawTools)
			}

			spawns := converseClaudeCodeMCPCatalogs(t, wf, node, ws, manager)
			if len(spawns) == 0 {
				t.Fatal("the stand-in CLI was never spawned")
			}
			named, strict := false, 0
			for _, spawn := range spawns {
				flag, catalog, _ := strings.Cut(spawn, " ")
				if flag == "strict" {
					strict++
				}
				if strings.Contains(catalog, `"`+converseMCPServer+`"`) {
					named = true
				}
			}
			if tc.shipped && strict != len(spawns) {
				t.Errorf("%d of %d claude_code spawn(s) carry --strict-mcp-config under ITERION_CLAUDE_CODE_STRICT_MCP=0: %q", strict, len(spawns), spawns)
			}
			if !tc.shipped && strict != 0 {
				t.Fatalf("control: the escape hatch did not lift --strict-mcp-config (%d spawn(s)) — the fixture proves nothing", strict)
			}
			catalogs := spawns
			if tc.shipped && named {
				t.Errorf("a claude_code spawn's --mcp-config names %q: %q", converseMCPServer, catalogs)
			}
			if !tc.shipped && !named {
				t.Fatalf("control: no claude_code spawn's --mcp-config names %q: %q", converseMCPServer, catalogs)
			}
		})
	}
}

// compileReviConverseAgent compiles the shipped bundle and returns its
// workflow and converse_agent.
func compileReviConverseAgent(t *testing.T) (*ir.Workflow, *ir.AgentNode) {
	t.Helper()
	u, err := unit.LoadDir(filepath.Join(reviConverseBundle, "main.bot"))
	if err != nil {
		t.Fatalf("load revi-converse: %v", err)
	}
	compiled := ir.Compile(u.Merged)
	if compiled.HasErrors() {
		t.Fatalf("compile revi-converse: %v", compiled.Diagnostics)
	}
	node, ok := compiled.Workflow.Nodes["converse_agent"].(*ir.AgentNode)
	if !ok {
		t.Fatalf("converse_agent is %T, want *ir.AgentNode", compiled.Workflow.Nodes["converse_agent"])
	}
	return compiled.Workflow, node
}

// converseMCPPeer serves one MCP tool over streamable HTTP and returns the
// endpoint.
func converseMCPPeer(t *testing.T) string {
	t.Helper()
	server := gomcp.NewServer(&gomcp.Implementation{Name: "webprobe", Version: "v0.0.1"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{
		Name: "scrape", Description: "Scrape a web page", InputSchema: map[string]any{"type": "object"},
	}, func(context.Context, *gomcp.CallToolRequest, any) (*gomcp.CallToolResult, any, error) {
		return &gomcp.CallToolResult{}, nil, nil
	})
	handler := gomcp.NewStreamableHTTPHandler(
		func(*http.Request) *gomcp.Server { return server },
		&gomcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	peer := httptest.NewServer(handler)
	t.Cleanup(peer.Close)
	return peer.URL
}

// converseMCPManager is the run's MCP manager over the catalog the
// preparation resolved.
func converseMCPManager(t *testing.T, wf *ir.Workflow) *mcp.Manager {
	t.Helper()
	catalog := map[string]*mcp.ServerConfig{}
	for name, srv := range wf.ResolvedMCPServers {
		catalog[name] = &mcp.ServerConfig{Name: srv.Name, Origin: mcp.OriginPlugin, Transport: mcp.FromIRTransport(srv.Transport), URL: srv.URL}
	}
	manager := mcp.NewManager(catalog, mcp.WithStartPolicy(mcp.StartAllServers))
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

// converseClawToolNames builds the node's claw task the way dispatch does —
// its resolved MCP set started through the run's manager — and returns the
// tool definitions the loop would send.
func converseClawToolNames(t *testing.T, wf *ir.Workflow, node ir.Node, ws string, manager *mcp.Manager) []string {
	t.Helper()
	reg := tool.NewRegistry()
	llm, ok := node.(ir.LLMNode)
	if !ok {
		t.Fatalf("%T is not an LLM node", node)
	}
	for _, name := range append(slices.Clone(llm.GetTools()), "todo_write") {
		if err := reg.RegisterBuiltin(name, name, nil, func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
			t.Fatal(err)
		}
	}
	e := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(ws), WithToolRegistry(reg), WithMCPManager(manager), WithLogger(iterlog.Nop()))
	f, err := extractBackendFields(node)
	if err != nil {
		t.Fatal(err)
	}
	built, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, nil)
	if err != nil {
		t.Fatalf("build the claw task: %v", err)
	}
	var names []string
	for _, def := range built.ToolDefs {
		names = append(names, def.Name)
	}
	return names
}

// converseStandInCLI records, per spawn, whether --strict-mcp-config was
// passed ("strict" or "loose") and the MCP catalog its --mcp-config names:
// the file's content when the flag names a file (a host spawn moves the
// inline document into one), the value itself otherwise, nothing for a spawn
// carrying no such flag. It answers the minimum stream-json the SDK needs to
// end a turn, once it has read the user message.
const converseStandInCLI = `#!/bin/sh
catalog=; prev=; strict=loose
for a in "$@"; do if [ "$prev" = --mcp-config ]; then catalog=$a; fi; if [ "$a" = --strict-mcp-config ]; then strict=strict; fi; prev=$a; done
printf '%s ' "$strict" >> "$CATALOG_LOG"
if [ -n "$catalog" ] && [ -f "$catalog" ]; then tr '\n' ' ' < "$catalog" >> "$CATALOG_LOG"; else printf '%s' "$catalog" | tr '\n' ' ' >> "$CATALOG_LOG"; fi
printf '\n' >> "$CATALOG_LOG"
case "$*" in *--input-format*)
	while read -r line; do case "$line" in *'"type":"user"'*) break ;; esac; done ;;
esac
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1","model":"fake","tools":[],"mcp_servers":[]}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"s1"}'
`

// converseClaudeCodeMCPCatalogs builds the node's claude_code task through
// the executor and runs it on the REAL claude_code backend against the
// stand-in CLI, returning per spawn its strictness and the MCP catalog it was
// handed ("strict <catalog>" assertions live in the callers; the strings are
// "strict|loose <catalog>").
func converseClaudeCodeMCPCatalogs(t *testing.T, wf *ir.Workflow, node ir.Node, ws string, manager *mcp.Manager) []string {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	dir := t.TempDir()
	cli := filepath.Join(dir, "claude")
	if err := os.WriteFile(cli, []byte(converseStandInCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "catalogs.log")
	t.Setenv("CATALOG_LOG", logPath)
	e := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(ws), WithMCPManager(manager), WithLogger(iterlog.Nop()))
	f, err := extractBackendFields(node)
	if err != nil {
		t.Fatal(err)
	}
	task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaudeCode, nil)
	if err != nil {
		t.Fatalf("build the claude_code task: %v", err)
	}
	task.Command = cli
	task.WorkDir = ws
	backend := &delegate.ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelDebug, io.Discard)}
	// The verdict is not the subject: the stand-in answers an empty result,
	// which no schema accepts. What the spawns were handed is.
	res, execErr := backend.Execute(context.Background(), task)
	t.Logf("claude_code Execute: err=%v, formatting pass used=%v", execErr, res.FormattingPassUsed)
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}
