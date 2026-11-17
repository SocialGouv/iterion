package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// reviewPrBundle is the shipped review-pr bundle, this package's depth.
const reviewPrBundle = "../../../bots/review-pr"

// reviewPrReviewerNodes compiles the shipped review-pr bundle, imports
// included, and returns the four reviewer judge nodes.
func reviewPrReviewerNodes(t *testing.T) (*ir.Workflow, []*ir.JudgeNode) {
	t.Helper()
	u := unittest.Whole(unit.LoadDir(filepath.Join(reviewPrBundle, "main.bot")))
	compiled := ir.Compile(u.Merged)
	if compiled.HasErrors() {
		t.Fatalf("compile review-pr: %v", compiled.Diagnostics)
	}
	var nodes []*ir.JudgeNode
	for _, id := range []string{
		"reviewer_claude", "reviewer_claude_glance", "reviewer_gpt", "reviewer_gpt_glance",
	} {
		n, ok := compiled.Workflow.Nodes[id].(*ir.JudgeNode)
		if !ok {
			t.Fatalf("%s is %T, want *ir.JudgeNode", id, compiled.Workflow.Nodes[id])
		}
		nodes = append(nodes, n)
	}
	return compiled.Workflow, nodes
}

// reviewPrMCPTaskForBackend builds one reviewer's task the way dispatch
// does, on the backend named, its resolved MCP set started through the run's
// manager.
func reviewPrMCPTaskForBackend(t *testing.T, wf *ir.Workflow, node ir.Node, backend string, ws string, manager *mcp.Manager) delegate.Task {
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
	task, err := e.buildTask(context.Background(), node, f, map[string]any{}, backend, nil)
	if err != nil {
		t.Fatalf("build the %s task: %v", backend, err)
	}
	return task
}

// TestReviewPrReviewersInheritNoAmbientMCPServer proves the four reviewer
// slots carry no MCP server on any backend a routing cell can move them to
// (#3283 S6): the platform's plugin servers ride every tooled node by
// doctrine, ~20k tokens of schemas per request that none of a reviewer's
// steps ever calls (measured 2026-11-13: $0.19-0.44 a run, zero mcp_* calls).
//
// An operator plugin contributes one ambient server (the platform's
// firecrawl, in production), resolved the way a run resolves it
// (mcp.PrepareWorkflow). Each shipped node's active MCP set is then EMPTY,
// claw builds its tool definitions without an mcp_ tool, the claude_code
// catalog names no server and keeps --strict-mcp-config under
// ITERION_CLAUDE_CODE_STRICT_MCP=0, and the codex task carries
// MCPInheritDisabled — the field the codex backend refuses on when its own
// configuration lists an enabled server
// (TestCodexRefusesAnOptedOutNodeItsConfigurationGivesServers,
// pkg/backend/delegate).
//
// The same node with its `mcp:` block removed is the control: it inherits
// the server everywhere — so a silent fixture (a plugin that did not load,
// a server that did not answer) cannot pass for an opted-out node.
//
// Mutations that redden it: drop any reviewer's `mcp: inherit: false` from
// bots/review-pr/lib/nodes.bot.
func TestReviewPrReviewersInheritNoAmbientMCPServer(t *testing.T) {
	// t.Parallel: t.Setenv below — the converse twin of this test runs serially for the same reason
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

	wf, nodes := reviewPrReviewerNodes(t)
	for _, node := range nodes {
		t.Run(node.ID, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				shipped bool
			}{
				{"shipped node", true},
				{"control: the node without its mcp block", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ws := t.TempDir()
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
						t.Errorf("active MCP servers = %v, want none", node.ActiveMCPServers)
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
					if tc.shipped && named {
						t.Errorf("a claude_code spawn's --mcp-config names %q: %q", converseMCPServer, spawns)
					}
					if !tc.shipped && !named {
						t.Fatalf("control: no claude_code spawn's --mcp-config names %q: %q", converseMCPServer, spawns)
					}

					codexTask := reviewPrMCPTaskForBackend(t, wf, node, delegate.BackendCodex, ws, manager)
					if tc.shipped && !codexTask.MCPInheritDisabled {
						t.Errorf("the codex task carries MCPInheritDisabled=false — codex's own configuration would start its servers unchecked")
					}
					if !tc.shipped && codexTask.MCPInheritDisabled {
						t.Fatal("control: the codex task of a node without its mcp block opts out anyway")
					}
				})
			}
		})
	}
}
