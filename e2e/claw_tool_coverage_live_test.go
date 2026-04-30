//go:build live

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLive_ClawToolCoverage exercises iterion's claw backend against a
// real Anthropic Haiku model and asserts that every claw tool family
// declared in RegisterClawAll is actually invoked by an LLM
// end-to-end (not just registered, not just dispatched in isolation by
// the in-process smoke test).
//
// The fixture (examples/claw_tool_coverage.iter) is structured as
// three sequential agents with explicit checklists in their user
// prompt:
//
//   - files_runner    — file ops + shell + LLM utilities + plan_mode
//                       (13 tools)
//   - mcp_runner      — MCP tools (greet/reverse) + MCP resources
//                       (list_mcp_resources, read_mcp_resource)
//                       (4 tools)
//   - lifecycle_runner — task / team / cron CRUD lifecycle
//                       (11 tools)
//
// Asserts:
//   - Run finishes successfully.
//   - Every expected tool name appears at least once in the
//     EventToolCalled stream.
//   - Output schema fields the agents populate from real tool results
//     match the deterministic values the test server / file ops
//     produce ("Hello, Iterion!", "setatS", probe text edited).
//
// Tools intentionally NOT covered (and why): see the file header in
// examples/claw_tool_coverage.iter.
//
// Cost on Haiku 4.5 across the three agents has been observed at
// ~$0.03 per run.
//
// Requires ANTHROPIC_API_KEY and a Go toolchain (to build the test
// MCP server binary).
func TestLive_ClawToolCoverage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "ANTHROPIC_API_KEY")
	requireBinaryInPath(t, "go")

	// Build the stdio MCP test server so list_mcp_resources +
	// read_mcp_resource have something real to discover.
	binPath := filepath.Join(t.TempDir(), "mcp_test_server")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./examples/mcp_test_server")
	buildCmd.Dir = ".."
	if out, berr := buildCmd.CombinedOutput(); berr != nil {
		t.Fatalf("build mcp_test_server: %v\n%s", berr, out)
	}
	t.Setenv("ITERION_MCP_TEST_BINARY", binPath)

	wf := compileFixture(t, "claw_tool_coverage.iter")

	workspaceDir, err := os.MkdirTemp("", "iterion-claw-cov-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}

	runID := "live-claw-tool-coverage"

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	// Build executor. PlanMode must be allocated explicitly so
	// enter_plan_mode / exit_plan_mode get registered (the same
	// defaulting newDefaultExecutor does in the CLI run path).
	reg := model.NewRegistry()
	logger := iterlog.New(iterlog.LevelInfo, os.Stderr)
	hooks := model.NewStoreEventHooks(s, runID, logger)
	backendReg := delegate.DefaultRegistry(logger)
	backendReg.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{}))

	mcpCatalog := make(map[string]*mcp.ServerConfig, len(wf.ResolvedMCPServers))
	for name, server := range wf.ResolvedMCPServers {
		expandedArgs := make([]string, len(server.Args))
		for i, a := range server.Args {
			expandedArgs[i] = os.ExpandEnv(a)
		}
		mcpCatalog[name] = &mcp.ServerConfig{
			Name:      server.Name,
			Transport: mcp.FromIRTransport(server.Transport),
			Command:   os.ExpandEnv(server.Command),
			Args:      expandedArgs,
			URL:       os.ExpandEnv(server.URL),
			Headers:   server.Headers,
		}
	}
	mcpManager := mcp.NewManager(mcpCatalog, mcp.WithLogger(logger))

	toolReg := tool.NewRegistry()
	planActive := false
	planDir := filepath.Join(storeDir, "plan-mode")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		t.Fatalf("mkdir plan-mode dir: %v", err)
	}
	clawDefaults := tool.ClawDefaults{
		Workspace: workspaceDir,
		PlanMode:  &clawtools.PlanModeState{Active: &planActive, Dir: planDir},
	}
	if err := tool.RegisterClawAll(toolReg, clawDefaults); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}

	executor := model.NewClawExecutor(reg, wf,
		model.WithBackendRegistry(backendReg),
		model.WithToolRegistry(toolReg),
		model.WithMCPManager(mcpManager),
		model.WithWorkDir(workspaceDir),
		model.WithEventHooks(hooks),
	)
	defer executor.Close()
	executor.SetVars(map[string]interface{}{"workspace_dir": workspaceDir})

	eng := runtime.New(wf, s, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if runErr := eng.Run(ctx, runID, map[string]interface{}{}); runErr != nil {
		t.Fatalf("Run error: %v", runErr)
	}
	r, _ := s.LoadRun(runID)
	if r.Status != store.RunStatusFinished {
		t.Fatalf("Expected run status 'finished', got %q", r.Status)
	}

	events, evtErr := s.LoadEvents(runID)
	if evtErr != nil {
		t.Fatalf("LoadEvents: %v", evtErr)
	}

	// Enumerate every tool name observed in EventToolCalled OR
	// EventToolError. The empirical question this test answers is
	// "did the LLM actually exercise this tool's dispatch path?";
	// a domain error from the tool (e.g. structured_output telling
	// the LLM "payload must not be empty") is still a successful
	// dispatch — the wiring worked, the tool ran, it produced a
	// meaningful response. iterion normalises mcp.<server>.<tool>
	// → mcp_<server>_<tool> on the wire, so the asserted set uses
	// the underscore form too.
	called := make(map[string]int)
	for _, evt := range events {
		if evt.Data == nil {
			continue
		}
		if evt.Type != store.EventToolCalled && evt.Type != store.EventToolError {
			continue
		}
		if name, _ := evt.Data["tool"].(string); name != "" {
			called[name]++
		}
	}
	t.Logf("distinct tools called: %d", len(called))
	for name, n := range called {
		t.Logf("  %s: %d", name, n)
	}

	expected := []string{
		// files_runner
		"bash", "read_file", "write_file", "file_edit", "glob", "grep",
		"sleep", "send_user_message", "todo_write", "tool_search",
		"structured_output", "enter_plan_mode", "exit_plan_mode",
		// mcp_runner
		"mcp_testsrv_greet", "mcp_testsrv_reverse",
		"list_mcp_resources", "read_mcp_resource",
		// lifecycle_runner
		"task_create", "task_get", "task_list",
		"team_create", "team_get", "team_list", "team_delete",
		"cron_create", "cron_get", "cron_list", "cron_delete",
	}
	missing := []string{}
	for _, name := range expected {
		if called[name] == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("expected %d tool names; %d missing: %v", len(expected), len(missing), missing)
	}

	// Verify the agent outputs captured the deterministic tool results.
	var greeting, reversed, probeText string
	for _, evt := range events {
		if evt.Type != store.EventNodeFinished || evt.Data == nil {
			continue
		}
		out, _ := evt.Data["output"].(map[string]interface{})
		if out == nil {
			continue
		}
		switch evt.NodeID {
		case "files_runner":
			probeText, _ = out["probe_text"].(string)
		case "mcp_runner":
			greeting, _ = out["greeting"].(string)
			reversed, _ = out["reversed"].(string)
		}
	}
	if !strings.Contains(greeting, "Iterion") {
		t.Errorf("mcp_runner greeting missing 'Iterion': %q", greeting)
	}
	if reversed != "setatS" {
		t.Errorf("mcp_runner reversed != \"setatS\": got %q", reversed)
	}
	if !strings.Contains(probeText, "edited-from-claw") {
		t.Errorf("files_runner probe_text did not capture file_edit result: %q", probeText)
	}

	logRunRecap(t, events)
}
