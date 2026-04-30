//go:build live

package e2e

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clawlsp "github.com/SocialGouv/claw-code-go/pkg/api/lsp"
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
//     (13 tools)
//   - mcp_runner      — MCP tools (greet/reverse) + MCP resources
//     (list_mcp_resources, read_mcp_resource)
//     (4 tools)
//   - lifecycle_runner — task / team / cron CRUD lifecycle
//     (11 tools)
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

	// ── Phase 4 fixtures ───────────────────────────────────────────
	//
	// Deterministic fixtures the aux_runner agent will exercise:
	//   - HTTP server : web_fetch GET /probe → "iterion-web-fetch-probe"
	//                   remote_trigger POST /trigger → 202 + "queued"
	//   - skill       : .claude/skills/probe-skill.md inside workspaceDir
	//   - notebook    : probe.ipynb inside workspaceDir (1 code cell)
	//   - image       : probe.png inside workspaceDir (1×1 transparent PNG)
	//   - config      : ClawDefaults.Config map with known keys

	auxHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/probe":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><h1>iterion-web-fetch-probe</h1><p>fixture content</p></body></html>`))
		case "/trigger":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"queued","probe":"iterion-remote-trigger-probe"}`))
		case "/brave-search":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
                "web": {
                    "results": [
                        {
                            "title": "iterion-web-search-probe",
                            "url": "https://example.com/iterion-probe",
                            "description": "iterion-web-search-marker"
                        }
                    ]
                }
            }`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(auxHTTP.Close)

	skillsDir := filepath.Join(workspaceDir, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	skillPath := filepath.Join(skillsDir, "probe-skill.md")
	const skillBody = "# Probe skill\n\niterion-skill-probe-marker\n"
	if err := os.WriteFile(skillPath, []byte(skillBody), 0o644); err != nil {
		t.Fatalf("write skill fixture: %v", err)
	}

	nbPath := filepath.Join(workspaceDir, "probe.ipynb")
	const nbBody = `{
  "cells": [
    {"cell_type": "code", "id": "probe-cell", "source": "print('iterion-notebook-probe-original')\n", "metadata": {}, "outputs": []}
  ],
  "metadata": {"kernelspec": {"name": "python3", "display_name": "Python 3"}},
  "nbformat": 4,
  "nbformat_minor": 5
}
`
	if err := os.WriteFile(nbPath, []byte(nbBody), 0o644); err != nil {
		t.Fatalf("write notebook fixture: %v", err)
	}

	pngPath := filepath.Join(workspaceDir, "probe.png")
	pngBytes, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
	)
	if err := os.WriteFile(pngPath, pngBytes, 0o644); err != nil {
		t.Fatalf("write png fixture: %v", err)
	}

	t.Setenv("ITERION_AUX_HTTP_URL", auxHTTP.URL)
	t.Setenv("BRAVE_API_KEY", "iterion-test-fixture-key")
	t.Setenv("CLAW_WEB_SEARCH_BRAVE_URL", auxHTTP.URL+"/brave-search")

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
	// Pre-register a fake "go" LSP server so the lsp tool's
	// diagnostics action returns deterministic empty results without
	// us having to spawn gopls. The server status is "connected" but
	// no transport is wired — the registry's Dispatch path produces a
	// canned response for diagnostics that the LLM can extract from.
	lspReg := clawlsp.NewRegistry()
	lspReg.Register("go", clawlsp.StatusConnected, &workspaceDir, nil)

	// Fixture .go file the lsp tool can reference (no analysis needed
	// — claw's registry pattern-matches the extension to language).
	lspGoPath := filepath.Join(workspaceDir, "probe.go")
	if err := os.WriteFile(lspGoPath, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write fixture .go: %v", err)
	}

	clawDefaults := tool.ClawDefaults{
		Workspace:        workspaceDir,
		PlanMode:         &clawtools.PlanModeState{Active: &planActive, Dir: planDir},
		MCPProvider:      mcpManager.ClawProvider(nil),
		IncludeWebSearch: true,
		LSP:              lspReg,
		Config: map[string]any{
			"probe_key":    "iterion-config-probe-value",
			"probe_number": 42,
		},
		AskUser: func(_ context.Context, q clawtools.Question) (clawtools.Answer, error) {
			// Inline auto-answer: free-text marker the test asserts on.
			return clawtools.Answer{FreeText: "iterion-ask-user-probe-answer"}, nil
		},
	}
	clawDefaults.Subagent = model.NewSubagentRunner(
		reg, toolReg, hooks, nil, "anthropic/claude-haiku-4-5-20251001",
	)
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
		// workers_runner — claw worker state machine (in-memory).
		"worker_create", "worker_observe", "worker_resolve_trust",
		"worker_await_ready", "worker_get", "worker_send_prompt",
		"worker_observe_completion", "worker_restart", "worker_terminate",
		// tasks_runner — task lifecycle tools beyond CRUD basics.
		"task_update", "task_output", "task_stop", "run_task_packet",
		// subagent_runner — claw `agent` tool dispatched into a real
		// child conversation (iterion-supplied SubagentRunner).
		"agent",
		// aux_runner — Phase 4a auxiliaries: 8 tools driven by deterministic
		// fixtures (httptest, .ipynb, .md skill, PNG, in-memory config).
		"config", "remote_trigger", "web_fetch", "mcp_auth",
		"skill", "repl", "notebook_edit", "read_image",
		// aux_runner — Phase 4b additions: web_search via Brave-URL
		// override + ask_user with inline auto-answer + lsp.
		"web_search", "ask_user", "lsp",
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
		probeText, toolSearchHit, structuredEcho      string
		greeting, reversed, resourceURI, resourceText string
		taskID, teamID, cronID                        string
		workerID, workerStatusAfterSend               string
		workerPromptAttempts                          float64
		tasksTaskID, tasksStoppedStatus               string
		tasksPacketObjective                          string
		subagentID, subagentText                      string
		auxConfigValue, auxWebFetchText               string
		auxTriggerProbe, auxMCPAuthStatus             string
		auxSkillText, auxREPLStdout                   string
		auxNotebookStatus, auxImageDescription        string
		auxTriggerStatus                              float64
		auxWebSearchText, auxAskUserAnswer            string
		auxLSPAction                                  string
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
		case "workers_runner":
			workerID, _ = out["worker_id"].(string)
			workerStatusAfterSend, _ = out["worker_status_after_send"].(string)
			workerPromptAttempts, _ = out["worker_prompt_attempts"].(float64)
		case "tasks_runner":
			tasksTaskID, _ = out["tasks_task_id"].(string)
			tasksStoppedStatus, _ = out["tasks_stopped_status"].(string)
			tasksPacketObjective, _ = out["tasks_packet_objective"].(string)
		case "subagent_runner":
			subagentID, _ = out["subagent_id"].(string)
			subagentText, _ = out["subagent_text"].(string)
		case "aux_runner":
			auxConfigValue, _ = out["config_value"].(string)
			auxTriggerStatus, _ = out["trigger_status"].(float64)
			auxTriggerProbe, _ = out["trigger_probe"].(string)
			auxWebFetchText, _ = out["web_fetch_text"].(string)
			auxMCPAuthStatus, _ = out["mcp_auth_status"].(string)
			auxSkillText, _ = out["skill_text"].(string)
			auxREPLStdout, _ = out["repl_stdout"].(string)
			auxNotebookStatus, _ = out["notebook_status"].(string)
			auxImageDescription, _ = out["image_description"].(string)
			auxWebSearchText, _ = out["web_search_text"].(string)
			auxAskUserAnswer, _ = out["ask_user_answer"].(string)
			auxLSPAction, _ = out["lsp_action"].(string)
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

	// workers_runner — the state machine actually transitioned through
	// trust → spawning → ready → running, then restart + terminate.
	if workerID == "" {
		t.Errorf("workers_runner.worker_id empty — worker_create return not exploited")
	}
	if workerStatusAfterSend != "running" {
		t.Errorf("workers_runner.worker_status_after_send != %q: got %q (worker_send_prompt did not transition state, or LLM failed to capture it)",
			"running", workerStatusAfterSend)
	}
	if workerPromptAttempts < 1 {
		t.Errorf("workers_runner.worker_prompt_attempts < 1: got %v (proves prompt_delivery_attempts not exploited)",
			workerPromptAttempts)
	}

	// tasks_runner — task_stop transitioned status, run_task_packet
	// echoed the objective.
	if tasksTaskID == "" {
		t.Errorf("tasks_runner.tasks_task_id empty — task_create (in this runner) return not exploited")
	}
	if tasksStoppedStatus != "stopped" {
		t.Errorf("tasks_runner.tasks_stopped_status != %q: got %q (task_stop transition not exploited)",
			"stopped", tasksStoppedStatus)
	}
	if tasksPacketObjective != "probe-objective" {
		t.Errorf("tasks_runner.tasks_packet_objective != %q: got %q (run_task_packet objective not exploited)",
			"probe-objective", tasksPacketObjective)
	}

	// subagent_runner — the agent tool was overridden with iterion's
	// real SubagentRunner; the child Haiku call must produce the probe
	// string and the parent must capture it via the tool result.
	if subagentID == "" {
		t.Errorf("subagent_runner.subagent_id empty — agent tool result not exploited")
	}
	if !strings.Contains(subagentText, "PROBE-OK-12345") {
		t.Errorf("subagent_runner.subagent_text missing probe (real subagent did not run, or output not captured): %q",
			subagentText)
	}

	// aux_runner — 8 fixtures driven via real LLM dispatch.
	if auxConfigValue != "iterion-config-probe-value" {
		t.Errorf("aux_runner.config_value != %q: got %q (config tool result not exploited)",
			"iterion-config-probe-value", auxConfigValue)
	}
	if int(auxTriggerStatus) != 202 {
		t.Errorf("aux_runner.trigger_status != 202: got %v (remote_trigger HTTP status not captured)",
			auxTriggerStatus)
	}
	if !strings.Contains(auxTriggerProbe, "iterion-remote-trigger-probe") {
		t.Errorf("aux_runner.trigger_probe missing fixture marker: %q (remote_trigger response body not parsed)",
			auxTriggerProbe)
	}
	if !strings.Contains(auxWebFetchText, "iterion-web-fetch-probe") {
		t.Errorf("aux_runner.web_fetch_text missing fixture marker: %q (web_fetch result not exploited)",
			auxWebFetchText)
	}
	if auxMCPAuthStatus != "connected" {
		t.Errorf("aux_runner.mcp_auth_status != %q: got %q (mcp_auth via ClawProvider should report stdio testsrv as connected)",
			"connected", auxMCPAuthStatus)
	}
	if !strings.Contains(auxSkillText, "iterion-skill-probe-marker") {
		t.Errorf("aux_runner.skill_text missing fixture marker: %q (skill body not exploited)",
			auxSkillText)
	}
	if !strings.Contains(auxREPLStdout, "4") {
		t.Errorf("aux_runner.repl_stdout missing %q: got %q (repl python did not run, or stdout not captured)",
			"4", auxREPLStdout)
	}
	if auxNotebookStatus != "success" {
		t.Errorf("aux_runner.notebook_status != %q: got %q (notebook_edit replace did not succeed)",
			"success", auxNotebookStatus)
	}
	if auxImageDescription == "" {
		t.Errorf("aux_runner.image_description empty (read_image returned no description, or LLM did not exploit it)")
	}
	if !strings.Contains(auxWebSearchText, "iterion-web-search-probe") {
		t.Errorf("aux_runner.web_search_text missing fixture marker: %q (Brave URL override / response not exploited)",
			auxWebSearchText)
	}
	if auxAskUserAnswer != "iterion-ask-user-probe-answer" {
		t.Errorf("aux_runner.ask_user_answer != %q: got %q (inline AskUser handler answer not captured by LLM)",
			"iterion-ask-user-probe-answer", auxAskUserAnswer)
	}
	if auxLSPAction != "diagnostics" {
		t.Errorf("aux_runner.lsp_action != %q: got %q (lsp Dispatch result not exploited by LLM)",
			"diagnostics", auxLSPAction)
	}

	logRunRecap(t, events)
}
