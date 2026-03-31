package e2e

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/benchmark"
	"github.com/SocialGouv/iterion/delegate"
	"github.com/SocialGouv/iterion/model"
	"github.com/SocialGouv/iterion/runtime"
	"github.com/SocialGouv/iterion/store"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// loadDotEnv reads a .env file from the project root and sets each KEY=VALUE
// pair via t.Setenv (automatically cleaned up after the test). Silently
// returns if the file does not exist.
func loadDotEnv(t *testing.T) {
	t.Helper()
	path := filepath.Join("..", ".env")
	f, err := os.Open(path)
	if err != nil {
		return // .env is optional
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		t.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
	}
}

// requireCLI skips the test if the given CLI binary is not found in PATH.
func requireCLI(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s CLI not found in PATH — skipping live delegation test", name)
	}
}

// ---------------------------------------------------------------------------
// Live E2E test — MCP delegation via claude-code & codex
// ---------------------------------------------------------------------------

// TestLive_DualParallel_MCPDelegation executes the pr_refine_dual_model_parallel_mcp
// workflow with real claude-code and codex CLI delegation. Claude nodes are
// delegated to the `claude` CLI, GPT nodes to the `codex` CLI. No direct API
// keys are needed — the CLIs handle their own authentication.
//
// Requires:
//   - `claude` and `codex` CLIs installed and in PATH
//   - The CLIs must be authenticated (API keys configured in their own config)
//
// Automatically skipped when CLIs are absent or in -short mode.
func TestLive_DualParallel_MCPDelegation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t) // optional: load .env if present for CLI auth
	requireCLI(t, "claude")
	requireCLI(t, "codex")

	// Compile the MCP delegation variant.
	// Use compileFixture (not StubSafe) since delegates handle their own tools.
	wf := compileFixture(t, "pr_refine_dual_model_parallel_mcp.iter")

	// No model resolution needed — delegated nodes use delegate: instead of model:.

	// Create executor with delegate registry.
	// A model registry is still needed for the executor constructor, but no
	// providers need to be registered since all nodes use delegation.
	reg := model.NewRegistry()
	delegateReg := delegate.DefaultRegistry()

	executor := model.NewGoaiExecutor(reg, wf,
		model.WithDelegateRegistry(delegateReg),
	)

	// Set workflow variables for prompt template resolution.
	executor.SetVars(map[string]interface{}{
		"pr_title":           "test: add unit tests for auth middleware",
		"pr_description":     "This PR adds comprehensive unit tests for the authentication middleware including token validation, session management, and error handling.",
		"base_ref":           "origin/main",
		"head_ref":           "HEAD",
		"review_rules":       "Check for test coverage, error handling, and code clarity. Ensure tests are deterministic and do not depend on external services.",
		"final_review_rules": "Verify all review findings have been addressed. Check that no regressions were introduced.",
	})

	// Create store and engine.
	s := tmpStore(t)
	eng := runtime.New(wf, s, executor)

	// Run with a generous timeout (delegation via CLI is slower than direct API).
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	runID := "live-dual-parallel-mcp"
	inputs := map[string]interface{}{
		"pr_title":           "test: add unit tests for auth middleware",
		"pr_description":     "This PR adds comprehensive unit tests for the authentication middleware including token validation, session management, and error handling.",
		"base_ref":           "origin/main",
		"head_ref":           "HEAD",
		"review_rules":       "Check for test coverage, error handling, and code clarity. Ensure tests are deterministic and do not depend on external services.",
		"final_review_rules": "Verify all review findings have been addressed. Check that no regressions were introduced.",
	}

	t.Log("Starting live dual-parallel MCP delegation workflow run...")
	start := time.Now()
	err := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run completed in %s", elapsed.Round(time.Second))

	// --- Assertions ---
	// The run may finish successfully or fail due to budget/loop limits.
	// Both are acceptable for a live test. Infrastructure errors are not.

	if err != nil {
		if errors.Is(err, runtime.ErrBudgetExceeded) {
			t.Logf("Run ended with budget exceeded (acceptable): %v", err)
		} else if errors.Is(err, runtime.ErrRunCancelled) {
			t.Fatalf("Run was cancelled (timeout?): %v", err)
		} else {
			t.Fatalf("Unexpected run error: %v", err)
		}
	}

	// Load run metadata.
	r, loadErr := s.LoadRun(runID)
	if loadErr != nil {
		t.Fatalf("Failed to load run: %v", loadErr)
	}
	t.Logf("Run status: %s", r.Status)

	if r.Status != store.RunStatusFinished && r.Status != store.RunStatusFailed {
		t.Errorf("Unexpected run status: %s (expected finished or failed)", r.Status)
	}

	// Load events.
	events, evtErr := s.LoadEvents(runID)
	if evtErr != nil {
		t.Fatalf("Failed to load events: %v", evtErr)
	}

	if !hasEvent(events, store.EventRunStarted) {
		t.Error("Missing run_started event")
	}

	// Verify both delegation backends were called.
	finishedNodes := eventNodeIDs(events, store.EventNodeFinished)
	claudeCalled := false
	codexCalled := false
	for _, id := range finishedNodes {
		if strings.HasPrefix(id, "claude_") {
			claudeCalled = true
		}
		if strings.HasPrefix(id, "gpt_") || strings.HasPrefix(id, "codex_") {
			codexCalled = true
		}
	}
	if !claudeCalled {
		t.Error("No claude_* node finished — claude-code delegation may have failed")
	}
	if !codexCalled {
		t.Error("No gpt_*/codex_* node finished — codex delegation may have failed")
	}

	// Verify metrics.
	metrics, mErr := benchmark.CollectMetrics(s, runID, "live-dual-parallel-mcp", "")
	if mErr != nil {
		t.Fatalf("Failed to collect metrics: %v", mErr)
	}

	t.Logf("Metrics: tokens=%d cost=$%.4f model_calls=%d iterations=%d duration=%s",
		metrics.TotalTokens, metrics.TotalCostUSD, metrics.ModelCalls, metrics.Iterations, metrics.DurationStr)

	if metrics.Iterations <= 0 {
		t.Error("Expected at least one iteration (node execution)")
	}
}

// ---------------------------------------------------------------------------
// Live E2E test — Todo App with dual-model MCP delegation
// ---------------------------------------------------------------------------

// TestLive_TodoApp_DualModel_MCP executes the todo_app_dual_model_mcp workflow
// with real claude-code and codex CLI delegation. The workflow designs, challenges,
// implements, and reviews a todo app as a single index.html file.
//
// The generated workspace directory is NOT cleaned up so the user can manually
// inspect the resulting index.html in a browser.
//
// Requires:
//   - `claude` and `codex` CLIs installed and in PATH
//   - The CLIs must be authenticated (API keys configured in their own config)
//
// Automatically skipped when CLIs are absent or in -short mode.
func TestLive_TodoApp_DualModel_MCP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireCLI(t, "codex")

	// Compile the todo app workflow fixture.
	wf := compileFixture(t, "todo_app_dual_model_mcp.iter")

	// Create a persistent workspace directory that survives the test so the
	// user can inspect the generated index.html.
	workspaceDir, err := os.MkdirTemp("", "iterion-todo-app-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	// Initialize a git repo in the workspace so that codex CLI accepts it.
	gitInit := exec.Command("git", "init", workspaceDir)
	if out, gitErr := gitInit.CombinedOutput(); gitErr != nil {
		t.Fatalf("git init failed: %v\n%s", gitErr, out)
	}

	// Create executor with delegate registry (no model providers needed — all
	// nodes use delegation). WorkDir ensures CLI subprocesses run in the
	// workspace directory.
	reg := model.NewRegistry()
	delegateReg := delegate.DefaultRegistry()
	executor := model.NewGoaiExecutor(reg, wf,
		model.WithDelegateRegistry(delegateReg),
		model.WithWorkDir(workspaceDir),
	)

	appDescription := "A single-page todo application in one index.html file. " +
		"Features: add todo items, mark as complete/incomplete (toggle), " +
		"delete items, filter by all/active/completed. " +
		"Use vanilla HTML, CSS, and JavaScript (no frameworks). " +
		"The design should be clean and modern."

	acceptanceCriteria := "1. Single index.html file with embedded CSS and JS\n" +
		"2. Add new todo items via text input and button\n" +
		"3. Toggle todo completion with checkbox\n" +
		"4. Delete individual todo items\n" +
		"5. Filter view: All, Active, Completed\n" +
		"6. Responsive design that works on mobile\n" +
		"7. Items persist in localStorage"

	executor.SetVars(map[string]interface{}{
		"app_description":    appDescription,
		"acceptance_criteria": acceptanceCriteria,
	})

	// Create store inside the workspace for easy post-run inspection.
	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}

	eng := runtime.New(wf, s, executor)

	// Generous timeout — delegation via CLI is slower than direct API.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	runID := "live-todo-app-dual-mcp"
	inputs := map[string]interface{}{
		"app_description":    appDescription,
		"acceptance_criteria": acceptanceCriteria,
		"previous_review":    "",
	}

	t.Log("Starting live todo app dual-model MCP workflow run...")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run completed in %s", elapsed.Round(time.Second))

	// The run may finish or hit budget limits — both are acceptable.
	if runErr != nil {
		if errors.Is(runErr, runtime.ErrBudgetExceeded) {
			t.Logf("Run ended with budget exceeded (acceptable): %v", runErr)
		} else if errors.Is(runErr, runtime.ErrRunCancelled) {
			t.Fatalf("Run was cancelled (timeout?): %v", runErr)
		} else {
			t.Fatalf("Unexpected run error: %v", runErr)
		}
	}

	// Load run metadata.
	r, loadErr := s.LoadRun(runID)
	if loadErr != nil {
		t.Fatalf("Failed to load run: %v", loadErr)
	}
	t.Logf("Run status: %s", r.Status)

	if r.Status != store.RunStatusFinished && r.Status != store.RunStatusFailed {
		t.Errorf("Unexpected run status: %s (expected finished or failed)", r.Status)
	}

	// Load events.
	events, evtErr := s.LoadEvents(runID)
	if evtErr != nil {
		t.Fatalf("Failed to load events: %v", evtErr)
	}

	if !hasEvent(events, store.EventRunStarted) {
		t.Error("Missing run_started event")
	}

	// Verify both delegation backends were invoked.
	finishedNodes := eventNodeIDs(events, store.EventNodeFinished)
	t.Logf("Finished nodes: %v", finishedNodes)

	claudeCalled := false
	codexCalled := false
	for _, id := range finishedNodes {
		if strings.Contains(id, "claude") {
			claudeCalled = true
		}
		if strings.Contains(id, "codex") {
			codexCalled = true
		}
	}
	if !claudeCalled {
		t.Error("No claude_* node finished — claude-code delegation may have failed")
	}
	if !codexCalled {
		t.Error("No codex_* node finished — codex delegation may have failed")
	}

	// Verify key workflow nodes executed.
	nodeSet := make(map[string]bool)
	for _, id := range finishedNodes {
		nodeSet[id] = true
	}
	for _, expected := range []string{"claude_implement", "codex_review"} {
		if !nodeSet[expected] {
			t.Errorf("Expected node %q to have finished", expected)
		}
	}

	// Check that index.html was generated.
	htmlPath := filepath.Join(workspaceDir, "index.html")
	if info, statErr := os.Stat(htmlPath); statErr != nil {
		t.Logf("WARNING: index.html not found at %s (agents may have written elsewhere)", htmlPath)
	} else {
		t.Logf("SUCCESS: index.html exists (%d bytes) at %s", info.Size(), htmlPath)
		t.Logf("Open in browser: file://%s", htmlPath)
	}

	// Collect and log metrics.
	metrics, mErr := benchmark.CollectMetrics(s, runID, "live-todo-app-dual-mcp", "")
	if mErr != nil {
		t.Fatalf("Failed to collect metrics: %v", mErr)
	}

	t.Logf("Metrics: tokens=%d cost=$%.4f model_calls=%d iterations=%d duration=%s",
		metrics.TotalTokens, metrics.TotalCostUSD, metrics.ModelCalls,
		metrics.Iterations, metrics.DurationStr)

	if metrics.Iterations <= 0 {
		t.Error("Expected at least one iteration")
	}
}
