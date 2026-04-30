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

	// Enumerate every tool name observed across EventToolCalled
	// (success — the tool ran, returned a non-error result) and
	// EventToolError (dispatched — the tool ran but returned a
	// domain error like "payload must not be empty"). Tracked
	// separately so the test can answer two empirical questions:
	//
	//   - did the LLM REACH each tool's dispatch path?  (called+error)
	//   - did each tool actually SUCCEED at least once?  (called > 0)
	//
	// iterion normalises mcp.<server>.<tool> → mcp_<server>_<tool>
	// on the wire, so the asserted set uses the underscore form too.
	called := make(map[string]int)
	errored := make(map[string]int)
	for _, evt := range events {
		if evt.Data == nil {
			continue
		}
		name, _ := evt.Data["tool"].(string)
		if name == "" {
			continue
		}
		switch evt.Type {
		case store.EventToolCalled:
			called[name]++
		case store.EventToolError:
			errored[name]++
		}
	}

	dispatched := map[string]struct{}{}
	for n := range called {
		dispatched[n] = struct{}{}
	}
	for n := range errored {
		dispatched[n] = struct{}{}
	}
	t.Logf("distinct tools dispatched: %d (succeeded once: %d)", len(dispatched), len(called))
	for name := range dispatched {
		t.Logf("  %s: ok=%d err=%d", name, called[name], errored[name])
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
	// Two-tier assertion: every tool must be dispatched, AND every
	// tool must succeed at least once. The dispatch tier catches
	// wiring regressions (registry miss, schema mismatch) ; the
	// success tier catches semantic regressions (tool stub returns
	// errors for valid input, prompt teaches the LLM to call the
	// tool incorrectly without recovery). The test logs both lists
	// so a regression's nature is obvious.
	missingDispatch := []string{}
	missingSuccess := []string{}
	for _, name := range expected {
		_, dispatchedOK := dispatched[name]
		if !dispatchedOK {
			missingDispatch = append(missingDispatch, name)
			continue
		}
		if called[name] == 0 {
			missingSuccess = append(missingSuccess, name)
		}
	}
	if len(missingDispatch) > 0 {
		t.Errorf("expected %d tools dispatched; %d never reached: %v",
			len(expected), len(missingDispatch), missingDispatch)
	}
	if len(missingSuccess) > 0 {
		t.Errorf("expected every tool to succeed at least once; %d only ever errored: %v",
			len(missingSuccess), missingSuccess)
	}

	// Verify each agent's output captured the deterministic tool
	// results — prove the LLM not only DISPATCHED each tool but
	// EXPLOITED its return value into the workflow's output schema.
	var (
		probeText, toolSearchHit, structuredEcho                      string
		greeting, reversed, resourceURI, resourceText                 string
		taskID, teamID, cronID                                        string
	)
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
			toolSearchHit, _ = out["tool_search_hit"].(string)
			structuredEcho, _ = out["structured_payload_echo"].(string)
		case "mcp_runner":
			greeting, _ = out["greeting"].(string)
			reversed, _ = out["reversed"].(string)
			resourceURI, _ = out["resource_uri"].(string)
			resourceText, _ = out["resource_text"].(string)
		case "lifecycle_runner":
			taskID, _ = out["task_id"].(string)
			teamID, _ = out["team_id"].(string)
			cronID, _ = out["cron_id"].(string)
		}
	}

	// files_runner — file ops + tool_search + structured_output
	// produced data the LLM exploited.
	if !strings.Contains(probeText, "edited-from-claw") {
		t.Errorf("files_runner.probe_text did not capture file_edit result: %q", probeText)
	}
	if toolSearchHit == "" {
		t.Errorf("files_runner.tool_search_hit empty — tool_search result not exploited by LLM")
	}
	if structuredEcho != "claw-coverage" {
		t.Errorf("files_runner.structured_payload_echo != %q: got %q (structured_output result not exploited)",
			"claw-coverage", structuredEcho)
	}

	// mcp_runner — discovery → use chain.
	if !strings.Contains(greeting, "Iterion") {
		t.Errorf("mcp_runner.greeting missing 'Iterion': %q", greeting)
	}
	if reversed != "setatS" {
		t.Errorf("mcp_runner.reversed != %q: got %q", "setatS", reversed)
	}
	if resourceURI != "iterion://test/golden.txt" {
		t.Errorf("mcp_runner.resource_uri != %q: got %q (list_mcp_resources URI not exploited by read_mcp_resource)",
			"iterion://test/golden.txt", resourceURI)
	}
	if !strings.Contains(resourceText, "iterion-mcp-test-resource") {
		t.Errorf("mcp_runner.resource_text missing fixture content: %q", resourceText)
	}

	// lifecycle_runner — every CRUD family produced an ID the LLM
	// captured (proves the create-tool's return was actually parsed
	// + used by subsequent get/list/delete calls).
	if taskID == "" {
		t.Errorf("lifecycle_runner.task_id empty — task_create return not exploited")
	}
	if teamID == "" {
		t.Errorf("lifecycle_runner.team_id empty — team_create return not exploited")
	}
	if cronID == "" {
		t.Errorf("lifecycle_runner.cron_id empty — cron_create return not exploited")
	}

	logRunRecap(t, events)
}
