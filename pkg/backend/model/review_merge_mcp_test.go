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
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The production GPT reviewer completed, then its merger failed resolving
// mcp.iterion_board.add_labels: the target repo's ambient MCP set excluded
// the host board server. Exercise the shipped node through both readers,
// not just the presence of its tools/capabilities declarations.
func TestReviewPrGPTMergeWithRepositoryMCP(t *testing.T) {
	t.Setenv(mcp.EnvAutoLoad, "1")
	u := unittest.Whole(unit.LoadDir("../../../bots/review-pr/main.bot"))
	compiled := ir.Compile(u.Merged)
	if compiled.HasErrors() {
		t.Fatalf("compile Revi: %v", compiled.Diagnostics)
	}
	wf := compiled.Workflow
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".mcp.json"), []byte(`{"mcpServers":{"repo_probe":{"command":"/not-started"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := mcp.PrepareWorkflow(wf, project, project); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ir.NodeActiveMCPServers(wf.Nodes["reviewer_gpt"]), "repo_probe") {
		t.Fatal("fixture did not activate the repository MCP server for the reviewer")
	}
	node := wf.Nodes["converge_gpt"]
	f, err := extractBackendFields(node)
	if err != nil {
		t.Fatal(err)
	}
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = board.Close() })
	reg := tool.NewRegistry()
	for _, name := range []string{"read_file", "write_file", "glob", "todo_write"} {
		if err := reg.RegisterBuiltin(name, name, nil, func(context.Context, json.RawMessage) (string, error) { return "ok", nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := tool.RegisterClawBoardTools(reg, &tool.BoardConfig{Store: board, Capabilities: boardops.AllCapabilities()}); err != nil {
		t.Fatal(err)
	}
	e := &ClawExecutor{
		toolRegistry: reg,
		logger:       iterlog.Nop(),
		mcpManager: mcp.NewManager(map[string]*mcp.ServerConfig{
			"repo_probe": {Command: "/not-started"},
		}, mcp.WithStartPolicy(mcp.StartAllServers)),
	}
	for _, backend := range []string{delegate.BackendClaw, delegate.BackendClaudeCode} {
		t.Run(backend, func(t *testing.T) {
			built, err := e.buildTask(context.Background(), node, f, map[string]any{}, backend, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(built.MCPServers) != 0 {
				t.Fatalf("merger inherited repository servers: %v", built.MCPServers)
			}
			if !slices.Equal(built.Capabilities, f.capabilities) {
				t.Fatalf("merger lost board capabilities: %v", built.Capabilities)
			}
			want := []string{
				"read_file", "write_file", "glob",
				"mcp__iterion_board__add_labels", "mcp__iterion_board__create_issue",
				"mcp__iterion_board__get_issue", "mcp__iterion_board__list_issues",
				"mcp__iterion_board__list_labels", "mcp__iterion_board__remove_labels",
				"mcp__iterion_board__set_labels",
			}
			if backend == delegate.BackendClaw {
				want = append(want, "todo_write")
			}
			slices.Sort(want)
			got := slices.Clone(built.AllowedTools)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("merger tool surface = %v, want %v", got, want)
			}
			if backend == delegate.BackendClaw {
				var names []string
				for _, def := range built.ToolDefs {
					names = append(names, def.Name)
				}
				slices.Sort(names)
				// claw exposes sanitized wire names; the declaration above
				// retains the MCP double-underscore spelling.
				wireNames := slices.Clone(want)
				for i, name := range wireNames {
					wireNames[i] = strings.ReplaceAll(name, "__", "_")
				}
				if !slices.Equal(names, wireNames) {
					t.Fatalf("resolved tools = %v, want %v", names, wireNames)
				}
			}
		})
	}
	names := e.assembleEffectiveTools(f, delegate.BackendClaw, f.capabilities, false)
	defs, _, err := e.resolveToolsForNode(context.Background(), node, names)
	if err != nil {
		t.Fatalf("Revi's GPT merger cannot resolve its granted tools beside repository MCP: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("merger lost all tools")
	}
	create, ok, err := e.resolveSingleToolForNode(context.Background(), node, "mcp__iterion_board__create_issue")
	if err != nil || !ok {
		t.Fatalf("resolve granted board creation: %v", err)
	}
	out, err := create.Execute(context.Background(), json.RawMessage(`{"title":"Review finding"}`))
	if err != nil || !strings.Contains(out, "Review finding") {
		t.Fatalf("granted board operation did not execute: %s / %v", out, err)
	}
}
