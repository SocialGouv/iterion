//go:build live

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/benchmark"
	"github.com/SocialGouv/iterion/cli"
	"github.com/SocialGouv/iterion/delegate"
	"github.com/SocialGouv/iterion/ir"
	iterlog "github.com/SocialGouv/iterion/log"
	"github.com/SocialGouv/iterion/mcp"
	"github.com/SocialGouv/iterion/model"
	"github.com/SocialGouv/iterion/runtime"
	"github.com/SocialGouv/iterion/store"
	"github.com/SocialGouv/iterion/tool"
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

// newLiveExecutor creates a ClawExecutor with all standard backends registered.
func newLiveExecutor(wf *ir.Workflow, s *store.RunStore, runID, workDir string) *model.ClawExecutor {
	reg := model.NewRegistry()
	logger := iterlog.New(iterlog.LevelDebug, os.Stderr)
	hooks := model.NewStoreEventHooks(s, runID, logger)

	backendReg := delegate.DefaultRegistry(logger)
	backendReg.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{}))

	return model.NewClawExecutor(reg, wf,
		model.WithBackendRegistry(backendReg),
		model.WithToolRegistry(tool.NewRegistry()),
		model.WithWorkDir(workDir),
		model.WithEventHooks(hooks),
	)
}

// installValidateSyntax copies the validate_syntax.js script into the
// workspace's _tools/ directory so tool nodes can invoke it.
func installValidateSyntax(t *testing.T, workspaceDir string) {
	t.Helper()
	src := filepath.Join("..", "examples", "validate_syntax.js")
	dst := filepath.Join(workspaceDir, "_tools", "validate_syntax.js")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir _tools: %v", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read validate_syntax.js: %v", err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("write validate_syntax.js: %v", err)
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
// Live E2E test — Dual-model plan/implement/review with round-robin
// ---------------------------------------------------------------------------

// TestLive_DualModel_PlanImplementReview executes the
// dual_model_plan_implement_review workflow which features:
//   - Parallel planning (Claude + Codex) → plan merge
//   - Parallel validation → judge acceptance
//   - Round-robin refinement loop (Claude ↔ Codex) if plan rejected
//   - Round-robin implementation (Claude ↔ Codex)
//   - Parallel review → verdict judge
//   - Outer loop back to planning if implementation rejected
//
// The generated workspace directory is NOT cleaned up so the user can manually
// inspect the resulting files.
//
// Requires:
//   - `claude` CLI installed and in PATH
//   - The CLIs must be authenticated
//
// Automatically skipped when CLIs are absent or in -short mode.
func TestLive_Lite_DualModel_PlanImplementReview(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")

	// Default model if not set via .env or environment.
	if os.Getenv("CLAUDE_MODEL") == "" {
		t.Setenv("CLAUDE_MODEL", "openai/gpt-5.5")
	}

	// Compile the fixture.
	wf := compileFixture(t, "dual_model_plan_implement_review.iter")

	// Create a persistent workspace directory.
	workspaceDir, err := os.MkdirTemp("", "iterion-plan-impl-review-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	// Initialize a git repo so claude_code can read git context (status/diff).
	gitInit := exec.Command("git", "init", workspaceDir)
	if out, gitErr := gitInit.CombinedOutput(); gitErr != nil {
		t.Fatalf("git init failed: %v\n%s", gitErr, out)
	}

	installValidateSyntax(t, workspaceDir)

	// Create store inside the workspace for easy post-run inspection.
	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}

	runID := "live-plan-impl-review"

	// Prepare MCP server catalog (resolves any project-level .mcp.json).
	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	executor := newLiveExecutor(wf, s, runID, workspaceDir)
	defer executor.Close()

	taskDescription := "An interactive Kanban task board in a single index.html file. " +
		"Use vanilla HTML, CSS, and JavaScript (no frameworks or CDNs). " +
		"The app provides a fully functional project management board with columns, draggable tasks, " +
		"persistence, filtering, theming, and data import/export capabilities."

	acceptanceCriteria := "=== Layer 1 - Foundation ===\n" +
		"1. Single index.html file with all CSS and JS embedded, no external dependencies or CDN links\n" +
		"2. Board layout with at least 4 default columns (Backlog, To Do, In Progress, Done) rendered as vertical lanes\n" +
		"3. CRUD operations for tasks: create via a form (title + description), edit inline, delete with confirmation\n" +
		"4. Drag and drop: tasks can be moved between columns using HTML5 drag-and-drop API (dragstart, dragover, drop events)\n" +
		"5. localStorage persistence: all board state (columns, tasks, positions) survives page reload\n" +
		"\n=== Layer 2 - Interactivity ===\n" +
		"6. Real-time search input that filters visible tasks by title/description with case-insensitive matching\n" +
		"7. Category/tag system: each task can have 1+ colored tags, tags are filterable via clickable chips\n" +
		"8. Due dates: date picker on tasks, overdue tasks highlighted with a red border or background\n" +
		"9. Column management: add new columns, rename columns via inline edit, reorder columns via drag-and-drop\n" +
		"10. Undo/redo system: at least 10 levels of undo/redo for task operations (Ctrl+Z / Ctrl+Shift+Z)\n" +
		"\n=== Layer 3 - Polish ===\n" +
		"11. Dark and light theme toggle with a visible button; smooth CSS transition (>= 0.3s) between themes; preference saved to localStorage\n" +
		"12. Keyboard shortcuts: N for new task, Delete/Backspace to remove selected task, Escape to close modals, arrow keys to navigate between tasks\n" +
		"13. Task counters displayed on each column header showing the number of tasks in that column, updated in real-time\n" +
		"14. Drag preview: a styled ghost element visible during drag operations (not the browser default)\n" +
		"15. Responsive layout: single-column stacked view on screens < 768px, horizontal scroll on desktop\n" +
		"\n=== Layer 4 - Advanced ===\n" +
		"16. Export/Import: export entire board to JSON file (download), import from JSON file (file input), validate schema on import\n" +
		"17. Statistics dashboard: a toggleable panel showing tasks per column (bar or pie chart rendered in SVG or Canvas), completion rate percentage, and average time in each column\n" +
		"18. Subtasks/checklists: each task can have a checklist of sub-items with checkboxes, progress shown as '3/5 done' on the card\n" +
		"19. Bulk operations: multi-select tasks (checkboxes), bulk move to column, bulk delete, bulk tag assignment\n" +
		"20. Time tracking: each task has an optional timer (start/stop), elapsed time displayed on the card in HH:MM:SS format, total time per column shown in statistics"

	executor.SetVars(map[string]interface{}{
		"workspace_dir": workspaceDir,
	})

	// Set up snapshot mechanism to capture index.html after each implementation.
	var snapshotMu sync.Mutex
	snapshotCount := 0
	snapshotsDir := filepath.Join(workspaceDir, "_snapshots")
	if err := os.MkdirAll(snapshotsDir, 0o755); err != nil {
		t.Fatalf("Failed to create snapshots dir: %v", err)
	}

	onFinished := func(nodeID string, output map[string]interface{}) {
		if nodeID != "claude_implement" && nodeID != "gpt_implement" {
			return
		}
		snapshotMu.Lock()
		defer snapshotMu.Unlock()
		snapshotCount++
		src := filepath.Join(workspaceDir, "index.html")
		dst := filepath.Join(snapshotsDir, fmt.Sprintf("index_v%d_%s.html", snapshotCount, nodeID))
		data, readErr := os.ReadFile(src)
		if readErr != nil {
			t.Logf("SNAPSHOT: could not read index.html after %s (v%d): %v", nodeID, snapshotCount, readErr)
			return
		}
		if writeErr := os.WriteFile(dst, data, 0o644); writeErr != nil {
			t.Logf("SNAPSHOT: could not write %s: %v", filepath.Base(dst), writeErr)
			return
		}
		t.Logf("SNAPSHOT: %s -> %s (%d bytes)", nodeID, filepath.Base(dst), len(data))
	}

	eng := runtime.New(wf, s, executor, runtime.WithOnNodeFinished(onFinished))

	// 5-hour timeout — planning + validation + implementation + review phases (ambitious criteria).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Hour)
	defer cancel()
	inputs := map[string]interface{}{
		"task_description":    taskDescription,
		"acceptance_criteria": acceptanceCriteria,
		"workspace_dir":       workspaceDir,
	}

	t.Log("Starting live dual-model plan/implement/review workflow run...")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run completed in %s", elapsed.Round(time.Second))

	// Accept finished, budget-exceeded, loop-exhausted, and timeout-killed outcomes.
	if runErr != nil {
		acceptable := false
		if errors.Is(runErr, runtime.ErrBudgetExceeded) {
			acceptable = true
		}
		var rtErr *runtime.RuntimeError
		if errors.As(runErr, &rtErr) {
			switch rtErr.Code {
			case runtime.ErrCodeBudgetExceeded, runtime.ErrCodeLoopExhausted:
				acceptable = true
			case runtime.ErrCodeExecutionFailed:
				// Context deadline exceeded kills delegate subprocesses.
				acceptable = true
			}
		}
		if acceptable {
			t.Logf("Run ended with acceptable error: %v", runErr)
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

	// Log all finished nodes for traceability.
	finishedNodes := eventNodeIDs(events, store.EventNodeFinished)
	t.Logf("Finished nodes: %v", finishedNodes)

	// Verify both agent perspectives were invoked (different backends).
	claudeNodeCalled := false
	gptNodeCalled := false
	for _, id := range finishedNodes {
		if strings.Contains(id, "claude") {
			claudeNodeCalled = true
		}
		if strings.Contains(id, "gpt") {
			gptNodeCalled = true
		}
	}
	if !claudeNodeCalled {
		t.Error("No claude_* node finished — agent A perspective may have failed")
	}
	if !gptNodeCalled {
		t.Error("No gpt_* node finished — agent B perspective may have failed")
	}

	// Verify key planning nodes executed.
	nodeSet := make(map[string]bool)
	for _, id := range finishedNodes {
		nodeSet[id] = true
	}
	for _, expected := range []string{"claude_plan", "gpt_plan", "merge_plans"} {
		if !nodeSet[expected] {
			t.Errorf("Expected node %q to have finished", expected)
		}
	}

	// Count implementation and review iterations.
	claudeImplCount := 0
	gptImplCount := 0
	for _, id := range finishedNodes {
		if id == "claude_implement" {
			claudeImplCount++
		}
		if id == "gpt_implement" {
			gptImplCount++
		}
	}
	t.Logf("Implementation iterations: claude_implement=%d gpt_implement=%d", claudeImplCount, gptImplCount)

	// Verify loop edge events exist.
	loopEdges := 0
	for _, evt := range events {
		if evt.Type == store.EventEdgeSelected && evt.Data != nil {
			if _, ok := evt.Data["loop"]; ok {
				loopEdges++
			}
		}
	}
	t.Logf("Loop edge events: %d", loopEdges)

	// Check that index.html was generated.
	htmlPath := filepath.Join(workspaceDir, "index.html")
	if info, statErr := os.Stat(htmlPath); statErr != nil {
		t.Logf("WARNING: index.html not found at %s (agents may have written elsewhere)", htmlPath)
	} else {
		t.Logf("SUCCESS: index.html exists (%d bytes) at %s", info.Size(), htmlPath)
		t.Logf("Open in browser: file://%s", htmlPath)
	}

	// Collect and log metrics.
	metrics, mErr := benchmark.CollectMetrics(s, runID, "live-plan-impl-review", "")
	if mErr != nil {
		t.Fatalf("Failed to collect metrics: %v", mErr)
	}

	t.Logf("Metrics: tokens=%d cost=$%.4f model_calls=%d iterations=%d duration=%s",
		metrics.TotalTokens, metrics.TotalCostUSD, metrics.ModelCalls,
		metrics.Iterations, metrics.DurationStr)

	if metrics.Iterations <= 0 {
		t.Error("Expected at least one iteration")
	}

	// If the workflow completed, verify the final verdict via events.
	// The review_judge node may not publish an artifact, so we check the
	// node_finished event data instead.
	if r.Status == store.RunStatusFinished {
		var verdictFound bool
		for i := len(events) - 1; i >= 0; i-- {
			evt := events[i]
			if evt.Type == store.EventNodeFinished && evt.NodeID == "review_judge" && evt.Data != nil {
				if output, ok := evt.Data["output"]; ok {
					if outMap, ok := output.(map[string]interface{}); ok {
						if approved, ok := outMap["approved"].(bool); ok && approved {
							verdictFound = true
							t.Logf("VERDICT (review_judge): approved=true, confidence=%v", outMap["confidence"])
						}
					}
				}
			}
		}
		if !verdictFound {
			// Fallback: try artifact
			verdict, vErr := s.LoadLatestArtifact(runID, "review_judge")
			if vErr != nil {
				t.Logf("WARNING: Workflow finished but could not find review_judge approved=true verdict in events or artifacts")
			} else {
				if approved, ok := verdict.Data["approved"].(bool); !ok || !approved {
					t.Errorf("Workflow finished but review_judge verdict approved=%v (expected true)", verdict.Data["approved"])
				} else {
					t.Logf("VERDICT (review_judge): approved=true, confidence=%v", verdict.Data["confidence"])
				}
			}
		}
	} else {
		t.Logf("Workflow did not complete (status=%s) — partial results available for inspection", r.Status)
	}

	// --- Progression Analysis ---
	t.Log("\n=== ARTIFACT PROGRESSION ===")

	// index.html snapshots
	snapshots, _ := os.ReadDir(snapshotsDir)
	if len(snapshots) > 0 {
		for _, snap := range snapshots {
			info, _ := snap.Info()
			if info != nil {
				t.Logf("  %s  (%d bytes)", snap.Name(), info.Size())
			}
		}
	} else {
		t.Log("  No index.html snapshots captured (agents may not have written the file)")
	}

	// Published artifact versions
	for _, nodeID := range []string{"claude_plan", "gpt_plan", "merge_plans", "claude_val", "gpt_val", "val_judge", "claude_implement", "gpt_implement", "claude_review", "gpt_review", "review_judge"} {
		art, artErr := s.LoadLatestArtifact(runID, nodeID)
		if artErr != nil {
			continue
		}
		summary := ""
		if s, ok := art.Data["summary"].(string); ok {
			if len(s) > 100 {
				summary = s[:100] + "..."
			} else {
				summary = s
			}
		}
		if summary != "" {
			t.Logf("  artifact %-20s v%d: %s", nodeID, art.Version, summary)
		} else {
			t.Logf("  artifact %-20s v%d", nodeID, art.Version)
		}
	}

	// --- Generate persistent report ---
	reportPath := filepath.Join(workspaceDir, "report.md")
	reportOpts := cli.ReportOptions{
		RunID:    runID,
		StoreDir: storeDir,
		Output:   reportPath,
	}
	reportPrinter := cli.NewPrinter(cli.OutputHuman)
	if reportErr := cli.RunReport(reportOpts, reportPrinter); reportErr != nil {
		t.Logf("WARNING: could not generate report: %v", reportErr)
	} else {
		t.Logf("Report written to %s", reportPath)
	}

	// --- Recapitulation: all steps and LLM discussions ---
	logRunRecap(t, events)
}

// ---------------------------------------------------------------------------
// Live E2E test — Session continuity review/fix
// ---------------------------------------------------------------------------

// TestLive_SessionContinuity_ReviewFix executes the session_review_fix
// workflow which demonstrates:
//   - Combined judge+merge (plan_judge_merge)
//   - Triple-role review nodes (review + verdict + fix plan)
//   - Session continuity: fix nodes resume their reviewer's CLI session
//   - LLM router to select the most relevant fix agent
//
// Requires:
//   - `claude` CLI installed and in PATH
//   - The CLIs must be authenticated
//
// Automatically skipped when CLIs are absent or in -short mode.
func TestLive_Lite_SessionContinuity_ReviewFix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")

	if os.Getenv("CLAUDE_MODEL") == "" {
		t.Setenv("CLAUDE_MODEL", "openai/gpt-5.5")
	}

	wf := compileFixture(t, "session_review_fix.iter")

	workspaceDir, err := os.MkdirTemp("", "iterion-session-review-fix-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	gitInit := exec.Command("git", "init", workspaceDir)
	if out, gitErr := gitInit.CombinedOutput(); gitErr != nil {
		t.Fatalf("git init failed: %v\n%s", gitErr, out)
	}

	installValidateSyntax(t, workspaceDir)

	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}

	runID := "live-session-review-fix"

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	executor := newLiveExecutor(wf, s, runID, workspaceDir)
	defer executor.Close()

	taskDescription := "A 'Code Review Roulette' game in a single index.html file. " +
		"Use vanilla HTML, CSS, and JavaScript (no frameworks or CDNs). " +
		"The app displays code snippets containing intentional bugs. The player must " +
		"find and click on the buggy line within a time limit to score points. " +
		"Features multiple difficulty levels, a scoring system, and a retro dev aesthetic."

	acceptanceCriteria := "=== Layer 1 - Core Game ===\n" +
		"1. Single index.html file with all CSS and JS embedded, no external dependencies or CDN links\n" +
		"2. Game screen displaying a code snippet (syntax-highlighted with <pre>/<code> and CSS) with at least 10-20 lines of code\n" +
		"3. At least 8 built-in code challenges across 3 languages (JavaScript, Python, Go) with one intentional bug per snippet\n" +
		"4. Each line of code is clickable; clicking the buggy line scores a point, clicking wrong deducts a point\n" +
		"5. Countdown timer (configurable: 30s/60s/90s) visible at all times, game ends when timer reaches 0\n" +
		"\n=== Layer 2 - Game Mechanics ===\n" +
		"6. Difficulty levels (Easy/Medium/Hard) that affect: number of lines, subtlety of bugs, and timer duration\n" +
		"7. Score display with running total, streak counter (consecutive correct answers), and best streak highlight\n" +
		"8. 'Hint' button that highlights the region (top/middle/bottom third) containing the bug, costs 1 point to use\n" +
		"9. After each answer (correct or wrong), show explanation of the bug with the fix for 3 seconds before next snippet\n" +
		"\n=== Layer 3 - Polish ===\n" +
		"10. Retro terminal/hacker aesthetic: dark background, green/amber monospace text, scanline CSS effect, CRT glow\n" +
		"11. localStorage persistence: high scores table (top 5) with player initials, date, and difficulty level\n" +
		"12. Game over screen showing: final score, accuracy percentage, best streak, and 'Play Again' button\n" +
		"13. Smooth animations: line hover highlight, correct/wrong answer feedback (flash green/red), timer pulse when < 10s\n" +
		"14. Responsive layout: playable on both desktop and mobile (touch-friendly line selection on small screens)"

	executor.SetVars(map[string]interface{}{
		"workspace_dir": workspaceDir,
	})

	// Snapshot mechanism.
	var snapshotMu sync.Mutex
	snapshotCount := 0
	snapshotsDir := filepath.Join(workspaceDir, "_snapshots")
	if err := os.MkdirAll(snapshotsDir, 0o755); err != nil {
		t.Fatalf("Failed to create snapshots dir: %v", err)
	}

	onFinished := func(nodeID string, output map[string]interface{}) {
		if nodeID != "implement" && nodeID != "claude_fix" && nodeID != "gpt_fix" {
			return
		}
		snapshotMu.Lock()
		defer snapshotMu.Unlock()
		snapshotCount++
		src := filepath.Join(workspaceDir, "index.html")
		dst := filepath.Join(snapshotsDir, fmt.Sprintf("index_v%d_%s.html", snapshotCount, nodeID))
		data, readErr := os.ReadFile(src)
		if readErr != nil {
			t.Logf("SNAPSHOT: could not read index.html after %s (v%d): %v", nodeID, snapshotCount, readErr)
			return
		}
		if writeErr := os.WriteFile(dst, data, 0o644); writeErr != nil {
			t.Logf("SNAPSHOT: could not write %s: %v", filepath.Base(dst), writeErr)
			return
		}
		t.Logf("SNAPSHOT: %s -> %s (%d bytes)", nodeID, filepath.Base(dst), len(data))
	}

	eng := runtime.New(wf, s, executor, runtime.WithOnNodeFinished(onFinished))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	inputs := map[string]interface{}{
		"task_description":    taskDescription,
		"acceptance_criteria": acceptanceCriteria,
		"workspace_dir":       workspaceDir,
	}

	t.Log("Starting live session continuity review/fix workflow run...")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run completed in %s", elapsed.Round(time.Second))

	if runErr != nil {
		acceptable := false
		if errors.Is(runErr, runtime.ErrBudgetExceeded) {
			acceptable = true
		}
		var rtErr *runtime.RuntimeError
		if errors.As(runErr, &rtErr) {
			switch rtErr.Code {
			case runtime.ErrCodeBudgetExceeded, runtime.ErrCodeLoopExhausted:
				acceptable = true
			case runtime.ErrCodeExecutionFailed:
				acceptable = true
			}
		}
		if acceptable {
			t.Logf("Run ended with acceptable error: %v", runErr)
		} else {
			t.Fatalf("Unexpected run error: %v", runErr)
		}
	}

	r, loadErr := s.LoadRun(runID)
	if loadErr != nil {
		t.Fatalf("Failed to load run: %v", loadErr)
	}
	t.Logf("Run status: %s", r.Status)

	if r.Status != store.RunStatusFinished && r.Status != store.RunStatusFailed {
		t.Errorf("Unexpected run status: %s (expected finished or failed)", r.Status)
	}

	events, evtErr := s.LoadEvents(runID)
	if evtErr != nil {
		t.Fatalf("Failed to load events: %v", evtErr)
	}

	if !hasEvent(events, store.EventRunStarted) {
		t.Error("Missing run_started event")
	}

	finishedNodes := eventNodeIDs(events, store.EventNodeFinished)
	t.Logf("Finished nodes: %v", finishedNodes)

	// Verify both agent perspectives were invoked.
	claudeNodeCalled := false
	gptNodeCalled := false
	for _, id := range finishedNodes {
		if strings.Contains(id, "claude") {
			claudeNodeCalled = true
		}
		if strings.Contains(id, "gpt") {
			gptNodeCalled = true
		}
	}
	if !claudeNodeCalled {
		t.Error("No claude_* node finished")
	}
	if !gptNodeCalled {
		t.Error("No gpt_* node finished")
	}

	// Verify key nodes executed.
	nodeSet := make(map[string]bool)
	for _, id := range finishedNodes {
		nodeSet[id] = true
	}
	for _, expected := range []string{"claude_plan", "gpt_plan", "plan_judge_merge", "implement"} {
		if !nodeSet[expected] {
			t.Errorf("Expected node %q to have finished", expected)
		}
	}

	// Check for review nodes.
	if !nodeSet["claude_review"] || !nodeSet["gpt_review"] {
		t.Error("Expected both review nodes to have finished")
	}

	// Check for session continuity: verify fix nodes ran (proves the fix loop fired).
	// Only claude_fix exercises session: inherit (claude_code path).
	// gpt_fix runs session: fresh on claw direct — no CLI session to inherit.
	fixNodeRan := nodeSet["claude_fix"] || nodeSet["gpt_fix"]
	if nodeSet["claude_fix"] {
		t.Log("SESSION CONTINUITY: claude_fix ran with session: inherit")
	} else if nodeSet["gpt_fix"] {
		t.Log("INFO: gpt_fix ran (session: fresh — claw direct has no CLI sessions)")
	} else if !fixNodeRan {
		t.Log("INFO: No fix nodes ran — implementation was approved on first review")
	}

	// Count fix iterations.
	claudeFixCount := 0
	gptFixCount := 0
	for _, id := range finishedNodes {
		switch id {
		case "claude_fix":
			claudeFixCount++
		case "gpt_fix":
			gptFixCount++
		}
	}
	t.Logf("Fix iterations: claude_fix=%d gpt_fix=%d", claudeFixCount, gptFixCount)

	// Check that index.html was generated.
	htmlPath := filepath.Join(workspaceDir, "index.html")
	if info, statErr := os.Stat(htmlPath); statErr != nil {
		t.Logf("WARNING: index.html not found at %s", htmlPath)
	} else {
		t.Logf("SUCCESS: index.html exists (%d bytes) at %s", info.Size(), htmlPath)
		t.Logf("Open in browser: file://%s", htmlPath)
	}

	// Collect metrics.
	metrics, mErr := benchmark.CollectMetrics(s, runID, "live-session-review-fix", "")
	if mErr != nil {
		t.Fatalf("Failed to collect metrics: %v", mErr)
	}

	t.Logf("Metrics: tokens=%d cost=$%.4f model_calls=%d iterations=%d duration=%s",
		metrics.TotalTokens, metrics.TotalCostUSD, metrics.ModelCalls,
		metrics.Iterations, metrics.DurationStr)

	// Snapshots.
	snapshots, _ := os.ReadDir(snapshotsDir)
	if len(snapshots) > 0 {
		t.Log("\n=== ARTIFACT PROGRESSION ===")
		for _, snap := range snapshots {
			info, _ := snap.Info()
			if info != nil {
				t.Logf("  %s  (%d bytes)", snap.Name(), info.Size())
			}
		}
	}

	// Generate report.
	reportPath := filepath.Join(workspaceDir, "report.md")
	reportOpts := cli.ReportOptions{
		RunID:    runID,
		StoreDir: storeDir,
		Output:   reportPath,
	}
	reportPrinter := cli.NewPrinter(cli.OutputHuman)
	if reportErr := cli.RunReport(reportOpts, reportPrinter); reportErr != nil {
		t.Logf("WARNING: could not generate report: %v", reportErr)
	} else {
		t.Logf("Report written to %s", reportPath)
	}

	logRunRecap(t, events)
}

// ---------------------------------------------------------------------------
// Live E2E test — Full exhaustive DSL coverage
// ---------------------------------------------------------------------------

// TestLive_Full_ExhaustiveDSLCoverage exercises every DSL feature in a single
// workflow run: all node types, router modes, await strategies, session modes,
// edge features, tool nodes, human nodes (auto-answered), and budget tracking.
//
// Requires: `claude` CLI installed and authenticated.
func TestLive_Full_ExhaustiveDSLCoverage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")

	if os.Getenv("CLAUDE_MODEL") == "" {
		t.Setenv("CLAUDE_MODEL", "openai/gpt-5.5")
	}

	wf := compileFixture(t, "exhaustive_dsl_coverage.iter")

	workspaceDir, err := os.MkdirTemp("", "iterion-exhaustive-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	gitInit := exec.Command("git", "init", workspaceDir)
	if out, gitErr := gitInit.CombinedOutput(); gitErr != nil {
		t.Fatalf("git init failed: %v\n%s", gitErr, out)
	}

	installValidateSyntax(t, workspaceDir)

	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}

	runID := "live-exhaustive"

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	executor := newLiveExecutor(wf, s, runID, workspaceDir)
	defer executor.Close()

	executor.SetVars(map[string]interface{}{
		"workspace_dir": workspaceDir,
	})

	eng := runtime.New(wf, s, executor)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	inputs := map[string]interface{}{
		"task_description": "Write the number 42 to a file called answer.txt in the workspace directory.",
		"workspace_dir":    workspaceDir,
	}

	t.Log("Starting exhaustive DSL coverage workflow run...")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run completed in %s", elapsed.Round(time.Second))

	// Accept finished, budget-exceeded, loop-exhausted.
	if runErr != nil {
		acceptable := false
		if errors.Is(runErr, runtime.ErrBudgetExceeded) {
			acceptable = true
		}
		var rtErr *runtime.RuntimeError
		if errors.As(runErr, &rtErr) {
			switch rtErr.Code {
			case runtime.ErrCodeBudgetExceeded, runtime.ErrCodeLoopExhausted:
				acceptable = true
			case runtime.ErrCodeExecutionFailed:
				acceptable = true
			}
		}
		if acceptable {
			t.Logf("Run ended with acceptable error: %v", runErr)
		} else {
			t.Fatalf("Unexpected run error: %v", runErr)
		}
	}

	r, loadErr := s.LoadRun(runID)
	if loadErr != nil {
		t.Fatalf("Failed to load run: %v", loadErr)
	}
	t.Logf("Run status: %s", r.Status)

	events, evtErr := s.LoadEvents(runID)
	if evtErr != nil {
		t.Fatalf("Failed to load events: %v", evtErr)
	}

	if !hasEvent(events, store.EventRunStarted) {
		t.Error("Missing run_started event")
	}

	finishedNodes := eventNodeIDs(events, store.EventNodeFinished)
	t.Logf("Finished nodes: %v", finishedNodes)
	nodeSet := make(map[string]bool)
	for _, id := range finishedNodes {
		nodeSet[id] = true
	}

	// === A. Node Type Coverage ===

	// Tool node
	if !nodeSet["check_input"] {
		t.Error("COVERAGE: tool node 'check_input' did not finish")
	}

	// Human node (auto-answered by LLM)
	if !nodeSet["human_gate"] {
		t.Error("COVERAGE: human node 'human_gate' did not finish")
	}

	// Router fan_out_all
	if !nodeSet["dispatch"] {
		t.Error("COVERAGE: fan_out_all router 'dispatch' did not finish")
	}

	// Agent nodes (both backends)
	claudeAgent := false
	clawAgent := false
	for _, id := range finishedNodes {
		if id == "writer_a" || id == "refiner_a" || id == "refiner_b" || id == "extract_meta" || id == "merge" {
			claudeAgent = true
		}
		if id == "writer_b" {
			clawAgent = true
		}
	}
	if !claudeAgent {
		t.Error("COVERAGE: no claude_code agent node finished")
	}
	if !clawAgent {
		t.Error("COVERAGE: no claw direct agent node finished (writer_b)")
	}

	// Judge node
	if !nodeSet["quality_judge"] {
		t.Error("COVERAGE: judge node 'quality_judge' did not finish")
	}

	// === B. Human Node Auto-Answer (no pause) ===
	humanPaused := false
	for _, evt := range events {
		if evt.Type == store.EventRunPaused {
			humanPaused = true
		}
	}
	if humanPaused {
		t.Error("COVERAGE: run paused at human gate — expected auto-answer via interaction: llm")
	}

	// === C. Parallel Branches ===
	branchCount := countEventType(events, store.EventBranchStarted)
	if branchCount < 2 {
		t.Errorf("COVERAGE: expected >= 2 branch_started events (fan_out_all), got %d", branchCount)
	}
	t.Logf("Branch events: %d", branchCount)

	// === D. Await Strategies ===
	if nodeSet["merge"] {
		t.Log("COVERAGE: best_effort join (merge) completed")
	}
	if nodeSet["quality_judge"] {
		t.Log("COVERAGE: wait_all join (quality_judge) completed")
	}

	// === E. Session Modes ===
	if nodeSet["extract_meta"] {
		t.Log("COVERAGE: session fork (extract_meta) executed")
	}
	if nodeSet["merge"] {
		t.Log("COVERAGE: session artifacts_only (merge) executed")
	}

	// === F. Conditional Edges ===
	conditionEdges := 0
	for _, evt := range events {
		if evt.Type == store.EventEdgeSelected && evt.Data != nil {
			if _, ok := evt.Data["condition"]; ok {
				conditionEdges++
			}
		}
	}
	if conditionEdges == 0 {
		t.Error("COVERAGE: no conditional edge selections found")
	}
	t.Logf("Conditional edge events: %d", conditionEdges)

	// === G. Loop Events ===
	loopEdges := 0
	for _, evt := range events {
		if evt.Type == store.EventEdgeSelected && evt.Data != nil {
			if _, ok := evt.Data["loop"]; ok {
				loopEdges++
			}
		}
	}
	t.Logf("Loop edge events: %d", loopEdges)

	// === H. Fix Path Coverage ===
	if nodeSet["fix_dispatch"] {
		t.Log("COVERAGE: condition router (fix_dispatch) exercised")
	}
	if nodeSet["refine_selector"] {
		t.Log("COVERAGE: round_robin router (refine_selector) exercised")
	}
	if nodeSet["refiner_a"] || nodeSet["refiner_b"] {
		t.Log("COVERAGE: session inherit refiner executed")
	}
	if nodeSet["run_check"] {
		t.Log("COVERAGE: tool node with template refs (run_check) executed")
	}
	if nodeSet["recheck_judge"] {
		t.Log("COVERAGE: recheck judge executed")
	}

	// === I. Artifacts ===
	for _, artNode := range []string{"writer_a", "writer_b", "extract_meta", "quality_judge"} {
		art, artErr := s.LoadLatestArtifact(runID, artNode)
		if artErr != nil {
			if nodeSet[artNode] {
				t.Errorf("COVERAGE: node %q finished but artifact not found", artNode)
			}
		} else {
			t.Logf("Artifact %-20s v%d", artNode, art.Version)
		}
	}

	// === J. Metrics ===
	metrics, mErr := benchmark.CollectMetrics(s, runID, "live-exhaustive", "")
	if mErr != nil {
		t.Fatalf("Failed to collect metrics: %v", mErr)
	}
	t.Logf("Metrics: tokens=%d cost=$%.4f model_calls=%d iterations=%d duration=%s",
		metrics.TotalTokens, metrics.TotalCostUSD, metrics.ModelCalls,
		metrics.Iterations, metrics.DurationStr)

	// ModelCalls counts claw backend calls only; CLI backends (claude_code)
	// are tracked via Iterations. Verify tokens were consumed.
	if metrics.TotalTokens == 0 && metrics.Iterations == 0 {
		t.Error("Expected non-zero token consumption or iterations")
	}

	// === K. Workspace Validation ===
	answerPath := filepath.Join(workspaceDir, "answer.txt")
	if content, readErr := os.ReadFile(answerPath); readErr != nil {
		t.Logf("WARNING: answer.txt not found at %s", answerPath)
	} else {
		if strings.Contains(string(content), "42") {
			t.Logf("SUCCESS: answer.txt contains '42' (%d bytes)", len(content))
		} else {
			t.Logf("WARNING: answer.txt exists but does not contain '42': %q", string(content))
		}
	}

	// === L. Coverage Summary ===
	covered := []string{}
	if nodeSet["check_input"] {
		covered = append(covered, "tool_node")
	}
	if nodeSet["human_gate"] && !humanPaused {
		covered = append(covered, "human_auto_answer")
	}
	if nodeSet["dispatch"] {
		covered = append(covered, "router_fan_out_all")
	}
	if nodeSet["fix_dispatch"] {
		covered = append(covered, "router_condition")
	}
	if nodeSet["refine_selector"] {
		covered = append(covered, "router_round_robin")
	}
	if nodeSet["quality_judge"] {
		covered = append(covered, "judge_wait_all")
	}
	if nodeSet["merge"] {
		covered = append(covered, "await_best_effort", "session_artifacts_only")
	}
	if nodeSet["extract_meta"] {
		covered = append(covered, "session_fork", "reasoning_effort", "readonly")
	}
	if nodeSet["refiner_a"] || nodeSet["refiner_b"] {
		covered = append(covered, "session_inherit")
	}
	if loopEdges > 0 {
		covered = append(covered, "bounded_loop")
	}
	if conditionEdges > 0 {
		covered = append(covered, "conditional_edges")
	}
	if claudeAgent {
		covered = append(covered, "backend_claude_code")
	}
	if clawAgent {
		covered = append(covered, "backend_claw")
	}

	t.Logf("\n=== DSL COVERAGE: %d/%d features ===", len(covered), 15)
	for _, c := range covered {
		t.Logf("  [x] %s", c)
	}

	// Generate report.
	reportPath := filepath.Join(workspaceDir, "report.md")
	reportOpts := cli.ReportOptions{RunID: runID, StoreDir: storeDir, Output: reportPath}
	reportPrinter := cli.NewPrinter(cli.OutputHuman)
	if reportErr := cli.RunReport(reportOpts, reportPrinter); reportErr != nil {
		t.Logf("WARNING: could not generate report: %v", reportErr)
	} else {
		t.Logf("Report written to %s", reportPath)
	}

	logRunRecap(t, events)
}

// ---------------------------------------------------------------------------
// Live E2E test — claude_code session: inherit validation
// ---------------------------------------------------------------------------

// TestLive_Lite_SessionInheritValidation forces exactly one fix iteration via
// a "strict-first" judge that ALWAYS rejects on the first evaluation, so that
// the `fix` agent (declared `session: inherit`) actually runs with the
// upstream `_session_id` wired through the edge mapping.
//
// Asserts:
//  1. Run finishes successfully.
//  2. `fix` finished at least once.
//  3. `fix.output._session_id` equals `implement.output._session_id` —
//     proves claude_code received `--resume <id>` and continued the same
//     CLI session, rather than starting a fresh one.
//
// Requires: `claude` CLI installed and authenticated.
func TestLive_Lite_SessionInheritValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")

	if os.Getenv("CLAUDE_MODEL") == "" {
		t.Setenv("CLAUDE_MODEL", "openai/gpt-5.5")
	}

	wf := compileFixture(t, "session_inherit_validation.iter")

	workspaceDir, err := os.MkdirTemp("", "iterion-session-inherit-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	gitInit := exec.Command("git", "init", workspaceDir)
	if out, gitErr := gitInit.CombinedOutput(); gitErr != nil {
		t.Fatalf("git init failed: %v\n%s", gitErr, out)
	}

	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}

	runID := "live-session-inherit"

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	executor := newLiveExecutor(wf, s, runID, workspaceDir)
	defer executor.Close()

	executor.SetVars(map[string]interface{}{
		"workspace_dir": workspaceDir,
	})

	eng := runtime.New(wf, s, executor)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	inputs := map[string]interface{}{
		"task":          "Write the number 42 to a file named result.txt in the workspace directory. Just the two characters '4' and '2', no trailing newline.",
		"workspace_dir": workspaceDir,
	}

	t.Log("Starting live session-inherit validation run...")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run completed in %s", elapsed.Round(time.Second))

	if runErr != nil {
		t.Fatalf("Unexpected run error: %v", runErr)
	}

	r, loadErr := s.LoadRun(runID)
	if loadErr != nil {
		t.Fatalf("Failed to load run: %v", loadErr)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("Expected run status 'finished', got %q", r.Status)
	}

	events, evtErr := s.LoadEvents(runID)
	if evtErr != nil {
		t.Fatalf("Failed to load events: %v", evtErr)
	}

	finishedNodes := eventNodeIDs(events, store.EventNodeFinished)
	t.Logf("Finished nodes: %v", finishedNodes)

	nodeSet := make(map[string]bool)
	for _, id := range finishedNodes {
		nodeSet[id] = true
	}
	for _, expected := range []string{"implement", "judge_strict", "fix", "judge_recheck"} {
		if !nodeSet[expected] {
			t.Errorf("Expected node %q to have finished — strict-first judge or fix wiring may have failed", expected)
		}
	}

	// === Critical assertion: session_id continuity ===
	// Pull _session_id out of the implement and fix node_finished events.
	var implSessionID, fixSessionID string
	for _, evt := range events {
		if evt.Type != store.EventNodeFinished || evt.Data == nil {
			continue
		}
		out, ok := evt.Data["output"].(map[string]interface{})
		if !ok {
			continue
		}
		sid, _ := out["_session_id"].(string)
		switch evt.NodeID {
		case "implement":
			if implSessionID == "" {
				implSessionID = sid
			}
		case "fix":
			if fixSessionID == "" {
				fixSessionID = sid
			}
		}
	}

	if implSessionID == "" {
		t.Fatal("implement node finished without a _session_id — backend did not return one")
	}
	if fixSessionID == "" {
		t.Fatal("fix node finished without a _session_id — backend did not return one")
	}
	t.Logf("implement._session_id = %s", implSessionID)
	t.Logf("fix._session_id       = %s", fixSessionID)

	if fixSessionID != implSessionID {
		t.Errorf("SESSION INHERIT BROKEN: fix used a different CLI session.\n  implement: %s\n  fix:       %s\nThe fix agent declared session: inherit but did not resume the upstream session.", implSessionID, fixSessionID)
	} else {
		t.Logf("SESSION INHERIT VALIDATED: fix continued implement's session %s", implSessionID)
	}

	// Check that result.txt actually exists.
	resultPath := filepath.Join(workspaceDir, "result.txt")
	if content, readErr := os.ReadFile(resultPath); readErr != nil {
		t.Errorf("result.txt not found at %s: %v", resultPath, readErr)
	} else {
		t.Logf("result.txt content: %q (%d bytes)", string(content), len(content))
	}

	metrics, mErr := benchmark.CollectMetrics(s, runID, "live-session-inherit", "")
	if mErr == nil {
		t.Logf("Metrics: tokens=%d cost=$%.4f model_calls=%d iterations=%d duration=%s",
			metrics.TotalTokens, metrics.TotalCostUSD, metrics.ModelCalls,
			metrics.Iterations, metrics.DurationStr)
	}

	reportPath := filepath.Join(workspaceDir, "report.md")
	reportOpts := cli.ReportOptions{RunID: runID, StoreDir: storeDir, Output: reportPath}
	reportPrinter := cli.NewPrinter(cli.OutputHuman)
	if reportErr := cli.RunReport(reportOpts, reportPrinter); reportErr != nil {
		t.Logf("WARNING: could not generate report: %v", reportErr)
	} else {
		t.Logf("Report written to %s", reportPath)
	}

	logRunRecap(t, events)
}

// ---------------------------------------------------------------------------
// Live E2E test — claw backend comprehensive coverage
// ---------------------------------------------------------------------------

// TestLive_Lite_ClawComprehensive exercises the claw backend (in-process LLM)
// across paths that the other live tests do not reach:
//
//   - text-only generation (synthetic-tool wrapped)
//   - structured output with nested objects + enum
//   - text + tools + schema (the two-pass tool-loop -> JSON path)
//   - prompt cache write + cache read across two sequential calls
//   - multi-provider: openai/gpt-5.5 for phases 1-3, anthropic/claude-haiku-4-5 for 4-5
//
// Asserts:
//  1. All five phase nodes finished.
//  2. Phase 3 (tools_and_schema) emitted >= 3 EventToolCalled events for compute_sum.
//  3. Phase 5 (cache_hit) emitted at least one EventLLMStepFinished with
//     cache_read_tokens > 0 — proves the cache prefix written by phase 4 was read.
//  4. Each phase output carries the expected fields (haiku, project_name, results, answer...).
//
// Requires `claude` CLI is NOT used here — only ANTHROPIC_API_KEY and OPENAI_API_KEY.
func TestLive_Lite_ClawComprehensive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "OPENAI_API_KEY")
	requireEnv(t, "ANTHROPIC_API_KEY")

	wf := compileFixture(t, "claw_comprehensive_coverage.iter")

	workspaceDir, err := os.MkdirTemp("", "iterion-claw-comprehensive-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}

	runID := "live-claw-comprehensive"

	// Tool registry pre-populated with a synthetic compute_sum builtin.
	// Phase 3 (tools_and_schema) declares tools: [compute_sum] and the agent
	// must call it to produce the structured output.
	toolReg := tool.NewRegistry()
	computeSumSchema := []byte(`{"type":"object","properties":{"a":{"type":"integer","description":"first integer"},"b":{"type":"integer","description":"second integer"}},"required":["a","b"]}`)
	if regErr := toolReg.RegisterBuiltin("compute_sum",
		"Compute the integer sum of a and b. Always use this tool to add two integers; do not compute mentally.",
		computeSumSchema,
		func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				A int `json:"a"`
				B int `json:"b"`
			}
			if jerr := json.Unmarshal(input, &args); jerr != nil {
				return "", fmt.Errorf("compute_sum: invalid input: %w", jerr)
			}
			return fmt.Sprintf("%d", args.A+args.B), nil
		}); regErr != nil {
		t.Fatalf("RegisterBuiltin compute_sum: %v", regErr)
	}

	// Build executor with the populated tool registry.
	reg := model.NewRegistry()
	logger := iterlog.New(iterlog.LevelDebug, os.Stderr)
	hooks := model.NewStoreEventHooks(s, runID, logger)
	backendReg := delegate.DefaultRegistry(logger)
	backendReg.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{}))
	executor := model.NewClawExecutor(reg, wf,
		model.WithBackendRegistry(backendReg),
		model.WithToolRegistry(toolReg),
		model.WithWorkDir(workspaceDir),
		model.WithEventHooks(hooks),
	)
	defer executor.Close()

	// Build a stable filler so cache_warm and cache_hit see byte-identical
	// system blocks. Empirically, Claude 4-series models only engage the
	// prompt cache once the cacheable prefix exceeds several thousand
	// tokens — under ~3k tokens, cache_creation_input_tokens stays at 0.
	// ~88k chars ≈ 20k tokens, well above the observed threshold.
	var fillerSB strings.Builder
	fillerPara := "The Holocene is a geological epoch that began approximately 11,700 years before the present. It follows the Last Glacial Period, characterized by periodic glaciations and the eventual retreat of the ice sheets that covered large portions of the Northern Hemisphere. During the Holocene, human civilizations emerged, agricultural practices spread, and complex societies developed across multiple continents. "
	for i := 0; i < 200; i++ {
		fillerSB.WriteString(fillerPara)
	}
	cacheFiller := fillerSB.String()

	executor.SetVars(map[string]interface{}{
		"workspace_dir": workspaceDir,
		"cache_filler":  cacheFiller,
	})

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	eng := runtime.New(wf, s, executor)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	inputs := map[string]interface{}{
		"topic": "spring rain",
		"brief": "Build a tiny CLI tool that lists todo items grouped by priority. The implementation language is Go and the storage is JSON files.",
		"pairs": []map[string]int{
			{"a": 13, "b": 29},
			{"a": 7, "b": 5},
			{"a": 100, "b": 250},
		},
		"question_a": "What year was the first Linux kernel released?",
		"question_b": "What year did the World Wide Web become publicly available?",
	}

	t.Log("Starting live claw comprehensive coverage run...")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run completed in %s", elapsed.Round(time.Second))

	if runErr != nil {
		t.Fatalf("Unexpected run error: %v", runErr)
	}

	r, loadErr := s.LoadRun(runID)
	if loadErr != nil {
		t.Fatalf("Failed to load run: %v", loadErr)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("Expected run status 'finished', got %q", r.Status)
	}

	events, evtErr := s.LoadEvents(runID)
	if evtErr != nil {
		t.Fatalf("Failed to load events: %v", evtErr)
	}

	finishedNodes := eventNodeIDs(events, store.EventNodeFinished)
	t.Logf("Finished nodes: %v", finishedNodes)
	nodeSet := make(map[string]bool)
	for _, id := range finishedNodes {
		nodeSet[id] = true
	}

	// === Phase coverage ===
	for _, phase := range []string{"text_only", "structured_only", "tools_and_schema", "cache_warm", "cache_hit"} {
		if !nodeSet[phase] {
			t.Errorf("CLAW COVERAGE: phase %q did not finish", phase)
		}
	}

	// === Per-phase output sanity checks ===
	outputOf := func(nodeID string) map[string]interface{} {
		for _, evt := range events {
			if evt.Type == store.EventNodeFinished && evt.NodeID == nodeID && evt.Data != nil {
				if out, ok := evt.Data["output"].(map[string]interface{}); ok {
					return out
				}
			}
		}
		return nil
	}

	if out := outputOf("text_only"); out == nil {
		t.Error("text_only: no output captured")
	} else if haiku, _ := out["haiku"].(string); strings.TrimSpace(haiku) == "" {
		t.Errorf("text_only: empty haiku field, got %v", out)
	} else {
		t.Logf("text_only haiku: %q", haiku)
	}

	if out := outputOf("structured_only"); out == nil {
		t.Error("structured_only: no output captured")
	} else {
		name, _ := out["project_name"].(string)
		count, _ := out["total_phase_count"].(float64)
		phaseNames, _ := out["phase_names"].([]interface{})
		risks, _ := out["primary_risks"].([]interface{})
		if name == "" || count <= 0 || len(phaseNames) == 0 || len(risks) == 0 {
			t.Errorf("structured_only: invalid output (name=%q count=%v phases=%d risks=%d): %v",
				name, count, len(phaseNames), len(risks), out)
		} else {
			t.Logf("structured_only: project_name=%q phases=%d risks=%d", name, len(phaseNames), len(risks))
		}
	}

	if out := outputOf("tools_and_schema"); out == nil {
		t.Error("tools_and_schema: no output captured")
	} else {
		results := out["results"]
		total, _ := out["total"].(float64)
		count, _ := out["count"].(float64)
		// Expected: pairs (13+29, 7+5, 100+250) -> [42, 12, 350], total 404, count 3.
		if int(count) != 3 || int(total) != 404 {
			t.Errorf("tools_and_schema: expected count=3 total=404, got count=%v total=%v results=%v", count, total, results)
		} else {
			t.Logf("tools_and_schema: results=%v total=%v count=%v", results, total, count)
		}
	}

	for _, phase := range []string{"cache_warm", "cache_hit"} {
		if out := outputOf(phase); out == nil {
			t.Errorf("%s: no output captured", phase)
		} else if ans, _ := out["answer"].(string); strings.TrimSpace(ans) == "" {
			t.Errorf("%s: empty answer field, got %v", phase, out)
		} else {
			t.Logf("%s answer: %q", phase, ans)
		}
	}

	// === Tool call assertion (phase 3) ===
	toolCallCount := 0
	for _, evt := range events {
		if evt.Type != store.EventToolCalled || evt.Data == nil {
			continue
		}
		if evt.NodeID != "tools_and_schema" {
			continue
		}
		if name, _ := evt.Data["tool"].(string); name == "compute_sum" {
			toolCallCount++
		}
	}
	if toolCallCount < 3 {
		t.Errorf("CLAW TWO-PASS: expected >= 3 compute_sum tool calls in tools_and_schema, got %d", toolCallCount)
	} else {
		t.Logf("CLAW TWO-PASS VALIDATED: tools_and_schema invoked compute_sum %d times", toolCallCount)
	}

	// === Cache hit assertion (phase 5) ===
	// Iterate llm_step_finished events; the cache_hit node should have at
	// least one step where cache_read_tokens > 0.
	cacheReadFound := 0
	cacheReadTotal := 0
	for _, evt := range events {
		if evt.Type != store.EventLLMStepFinished || evt.Data == nil {
			continue
		}
		if evt.NodeID != "cache_hit" {
			continue
		}
		if v, ok := evt.Data["cache_read_tokens"]; ok {
			n := 0
			switch x := v.(type) {
			case int:
				n = x
			case int64:
				n = int(x)
			case float64:
				n = int(x)
			}
			if n > 0 {
				cacheReadFound++
				cacheReadTotal += n
			}
		}
	}
	if cacheReadFound == 0 {
		t.Errorf("CLAW PROMPT CACHE: cache_hit produced no step with cache_read_tokens > 0. The cache_warm pass should have written the cache; if this fails, either the system block size dropped below Claude 4's eligibility threshold or the cache_control marker isn't reaching the wire (run model/cache_marshal_test.go to confirm marshal contract).")
	} else {
		t.Logf("CLAW PROMPT CACHE VALIDATED: cache_hit read %d cached tokens across %d step(s)", cacheReadTotal, cacheReadFound)
	}

	// Also verify phase 4 wrote cache (cache_write_tokens > 0).
	cacheWriteSeen := false
	for _, evt := range events {
		if evt.Type != store.EventLLMStepFinished || evt.Data == nil || evt.NodeID != "cache_warm" {
			continue
		}
		if v, ok := evt.Data["cache_write_tokens"]; ok {
			n := 0
			switch x := v.(type) {
			case int:
				n = x
			case int64:
				n = int(x)
			case float64:
				n = int(x)
			}
			if n > 0 {
				cacheWriteSeen = true
				t.Logf("cache_warm wrote %d cache tokens", n)
				break
			}
		}
	}
	if !cacheWriteSeen {
		t.Logf("note: cache_warm did not report cache_write_tokens > 0 (cache may have already existed from a prior run)")
	}

	metrics, mErr := benchmark.CollectMetrics(s, runID, "live-claw-comprehensive", "")
	if mErr == nil {
		t.Logf("Metrics: tokens=%d cost=$%.4f model_calls=%d iterations=%d duration=%s",
			metrics.TotalTokens, metrics.TotalCostUSD, metrics.ModelCalls,
			metrics.Iterations, metrics.DurationStr)
	}

	reportPath := filepath.Join(workspaceDir, "report.md")
	reportOpts := cli.ReportOptions{RunID: runID, StoreDir: storeDir, Output: reportPath}
	reportPrinter := cli.NewPrinter(cli.OutputHuman)
	if reportErr := cli.RunReport(reportOpts, reportPrinter); reportErr != nil {
		t.Logf("WARNING: could not generate report: %v", reportErr)
	} else {
		t.Logf("Report written to %s", reportPath)
	}

	logRunRecap(t, events)
}

// ---------------------------------------------------------------------------
// Live E2E test — claw with built-in filesystem/shell tools
// ---------------------------------------------------------------------------

// TestLive_Lite_ClawBuiltinTools exercises the new tool.RegisterClawBuiltins
// helper that wires claw-code-go's built-in tools (read_file, write_file,
// bash, glob, grep, file_edit, web_fetch) into iterion's tool registry,
// closing the "claude_code has tools out of the box, claw doesn't" gap.
//
// A claw-direct agent is given a small Go-development task that requires
// real file IO + shell execution (write hello.go, run `go run hello.go`,
// capture output, read file back).
//
// Asserts:
//  1. Run finishes successfully.
//  2. hello.go exists in the workspace.
//  3. The agent's reported bash output matches "claw is operational".
//  4. EventToolCalled events show >= 1 write_file and >= 1 bash call.
//
// Requires ANTHROPIC_API_KEY (the agent runs on claude-haiku-4-5).
func TestLive_Lite_ClawBuiltinTools(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "ANTHROPIC_API_KEY")
	requireBinaryInPath(t, "go")

	wf := compileFixture(t, "claw_builtin_tools.iter")

	workspaceDir, err := os.MkdirTemp("", "iterion-claw-builtin-tools-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	// Initialise a tiny go.mod so `go run hello.go` works without GOPATH.
	if werr := os.WriteFile(filepath.Join(workspaceDir, "go.mod"),
		[]byte("module hello\n\ngo 1.21\n"), 0o644); werr != nil {
		t.Fatalf("write go.mod: %v", werr)
	}

	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}

	runID := "live-claw-builtin-tools"

	// Tool registry seeded with claw built-ins.
	toolReg := tool.NewRegistry()
	if regErr := tool.RegisterClawBuiltins(toolReg, workspaceDir); regErr != nil {
		t.Fatalf("RegisterClawBuiltins: %v", regErr)
	}

	reg := model.NewRegistry()
	logger := iterlog.New(iterlog.LevelDebug, os.Stderr)
	hooks := model.NewStoreEventHooks(s, runID, logger)
	backendReg := delegate.DefaultRegistry(logger)
	backendReg.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{}))
	executor := model.NewClawExecutor(reg, wf,
		model.WithBackendRegistry(backendReg),
		model.WithToolRegistry(toolReg),
		model.WithWorkDir(workspaceDir),
		model.WithEventHooks(hooks),
	)
	defer executor.Close()
	executor.SetVars(map[string]interface{}{
		"workspace_dir": workspaceDir,
	})

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	eng := runtime.New(wf, s, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	t.Log("Starting live claw built-in tools run...")
	start := time.Now()
	runErr := eng.Run(ctx, runID, map[string]interface{}{
		"workspace_dir": workspaceDir,
	})
	elapsed := time.Since(start)
	t.Logf("Run completed in %s", elapsed.Round(time.Second))

	if runErr != nil {
		t.Fatalf("Unexpected run error: %v", runErr)
	}
	r, loadErr := s.LoadRun(runID)
	if loadErr != nil {
		t.Fatalf("Failed to load run: %v", loadErr)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("Expected run status 'finished', got %q", r.Status)
	}

	events, evtErr := s.LoadEvents(runID)
	if evtErr != nil {
		t.Fatalf("Failed to load events: %v", evtErr)
	}

	// Tool call counts.
	toolCounts := map[string]int{}
	for _, evt := range events {
		if evt.Type != store.EventToolCalled || evt.Data == nil {
			continue
		}
		if evt.NodeID != "builder" {
			continue
		}
		if name, _ := evt.Data["tool"].(string); name != "" {
			toolCounts[name]++
		}
	}
	t.Logf("Tool call counts: %v", toolCounts)

	if toolCounts["write_file"] < 1 {
		t.Errorf("CLAW BUILTIN TOOLS: expected >= 1 write_file call, got %d", toolCounts["write_file"])
	}
	if toolCounts["bash"] < 1 {
		t.Errorf("CLAW BUILTIN TOOLS: expected >= 1 bash call, got %d", toolCounts["bash"])
	}

	// hello.go must exist in workspace.
	helloPath := filepath.Join(workspaceDir, "hello.go")
	helloBytes, hErr := os.ReadFile(helloPath)
	if hErr != nil {
		t.Errorf("hello.go not found at %s: %v", helloPath, hErr)
	} else {
		t.Logf("hello.go (%d bytes):\n%s", len(helloBytes), string(helloBytes))
	}

	// Agent output should report applied=true and bash_output containing the marker.
	for _, evt := range events {
		if evt.Type != store.EventNodeFinished || evt.NodeID != "builder" || evt.Data == nil {
			continue
		}
		out, ok := evt.Data["output"].(map[string]interface{})
		if !ok {
			continue
		}
		applied, _ := out["applied"].(bool)
		bashOut, _ := out["bash_output"].(string)
		toolsUsed, _ := out["tools_used"].([]interface{})
		t.Logf("builder.applied = %v", applied)
		t.Logf("builder.bash_output = %q", bashOut)
		t.Logf("builder.tools_used = %v", toolsUsed)
		if !applied {
			t.Error("builder reported applied=false")
		}
		if !strings.Contains(bashOut, "claw is operational") {
			t.Errorf("bash_output missing marker 'claw is operational', got %q", bashOut)
		}
		if len(toolsUsed) < 2 {
			t.Errorf("expected tools_used to mention >= 2 distinct tools, got %v", toolsUsed)
		}
		break
	}

	metrics, mErr := benchmark.CollectMetrics(s, runID, "live-claw-builtin-tools", "")
	if mErr == nil {
		t.Logf("Metrics: tokens=%d cost=$%.4f model_calls=%d iterations=%d duration=%s",
			metrics.TotalTokens, metrics.TotalCostUSD, metrics.ModelCalls,
			metrics.Iterations, metrics.DurationStr)
	}

	logRunRecap(t, events)
}

// ---------------------------------------------------------------------------
// Live E2E test — reasoning_effort propagation
// ---------------------------------------------------------------------------

// TestLive_Lite_ClawReasoningEffort verifies that a node's reasoning_effort
// field propagates from the .iter declaration to the request body sent to
// the provider, by inspecting the EventLLMRequest entry in the store.
//
// Asserts that the EventLLMRequest event for the `thinker` node contains
// reasoning_effort=high in its data — proving the chain
//
//	ir.Node.ReasoningEffort
//	  → delegate.Task.ReasoningEffort
//	    → providerOptsForNode → opts.ProviderOptions["reasoning_effort"]
//	      → fireOnRequest → RequestInfo.ReasoningEffort
//	        → LLMRequestInfo.ReasoningEffort
//	          → store EventLLMRequest.data["reasoning_effort"]
//
// is intact end-to-end.
func TestLive_Lite_ClawReasoningEffort(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "OPENAI_API_KEY")

	wf := compileFixture(t, "claw_reasoning_effort.iter")

	workspaceDir, err := os.MkdirTemp("", "iterion-claw-reasoning-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}

	runID := "live-claw-reasoning"
	executor := newLiveExecutor(wf, s, runID, workspaceDir)
	defer executor.Close()

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	eng := runtime.New(wf, s, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Don't fail on the API call result — OpenAI's /v1/chat/completions
	// rejects reasoning_effort+tools on gpt-5.5 (requires /v1/responses,
	// a separate fix in claw-code-go's openai provider). The point of this
	// test is to verify iterion's *propagation*, which happens before the
	// API actually replies. The event we assert on is emitted by
	// fireOnRequest() at the start of every call.
	runErr := eng.Run(ctx, runID, map[string]interface{}{
		"question": "Briefly: why is the sky blue?",
	})
	if runErr != nil {
		t.Logf("note: run did not finish cleanly (%v) — checking propagation via events anyway", runErr)
	}

	events, evtErr := s.LoadEvents(runID)
	if evtErr != nil {
		t.Fatalf("LoadEvents: %v", evtErr)
	}

	// Assert: at least one llm_request event for `thinker` carries
	// reasoning_effort = "high".
	found := false
	for _, evt := range events {
		if evt.Type != store.EventLLMRequest || evt.NodeID != "thinker" || evt.Data == nil {
			continue
		}
		if re, _ := evt.Data["reasoning_effort"].(string); re == "high" {
			found = true
			t.Logf("REASONING_EFFORT VALIDATED: thinker llm_request carries reasoning_effort=%q (model=%v)",
				re, evt.Data["model"])
			break
		}
	}
	if !found {
		t.Errorf("REASONING_EFFORT: no EventLLMRequest for 'thinker' carried reasoning_effort=high — propagation broken")
	}

	logRunRecap(t, events)
}

// ---------------------------------------------------------------------------
// Live E2E test — MCP tool discovery + invocation through claw
// ---------------------------------------------------------------------------

// TestLive_Lite_ClawMCP exercises iterion's MCP integration end-to-end:
// it builds a tiny stdio MCP server (examples/mcp_test_server) that exposes
// two trivial tools (greet, reverse), declares it via `mcp_server testsrv`
// in the workflow, and asks a claw agent to call BOTH tools.
//
// Asserts:
//  1. Run finishes successfully.
//  2. EventToolCalled events show invocations of `mcp.testsrv.greet` and
//     `mcp.testsrv.reverse`.
//  3. Agent output greeting == "Hello, Iterion!" and reversed == "setatS"
//     — proving the tool results made it back into the model context.
//
// Requires: ANTHROPIC_API_KEY + go in PATH (used to compile the server).
func TestLive_Lite_ClawMCP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "ANTHROPIC_API_KEY")
	requireBinaryInPath(t, "go")

	// Build the stdio MCP server.
	binPath := filepath.Join(t.TempDir(), "mcp_test_server")
	buildCmd := exec.Command("go", "build", "-o", binPath, "./examples/mcp_test_server")
	buildCmd.Dir = ".."
	if out, berr := buildCmd.CombinedOutput(); berr != nil {
		t.Fatalf("build mcp_test_server: %v\n%s", berr, out)
	}
	t.Setenv("ITERION_MCP_TEST_BINARY", binPath)

	wf := compileFixture(t, "claw_mcp.iter")

	workspaceDir, err := os.MkdirTemp("", "iterion-claw-mcp-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}

	runID := "live-claw-mcp"

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	// Build executor with an MCP manager so the testsrv stdio server is
	// connected on first tool call and its tools are registered as
	// mcp.testsrv.* in the registry.
	reg := model.NewRegistry()
	logger := iterlog.New(iterlog.LevelDebug, os.Stderr)
	hooks := model.NewStoreEventHooks(s, runID, logger)
	backendReg := delegate.DefaultRegistry(logger)
	backendReg.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{}))

	mcpCatalog := make(map[string]*mcp.ServerConfig, len(wf.ResolvedMCPServers))
	for name, server := range wf.ResolvedMCPServers {
		// Expand env vars in stdio command/args so workflows can refer to
		// test-provided binaries via ${ITERION_MCP_TEST_BINARY} etc.
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

	executor := model.NewClawExecutor(reg, wf,
		model.WithBackendRegistry(backendReg),
		model.WithToolRegistry(tool.NewRegistry()),
		model.WithMCPManager(mcpManager),
		model.WithWorkDir(workspaceDir),
		model.WithEventHooks(hooks),
	)
	defer executor.Close()
	executor.SetVars(map[string]interface{}{
		"workspace_dir": workspaceDir,
	})

	eng := runtime.New(wf, s, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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

	// Tool call counts. iterion normalises mcp.<server>.<tool> to
	// mcp_<server>_<tool> for the wire because providers reject dots in
	// tool names. The original form is what the workflow declares; the
	// underscore form is what shows up in events.
	greetCalls, reverseCalls := 0, 0
	for _, evt := range events {
		if evt.Type != store.EventToolCalled || evt.Data == nil {
			continue
		}
		name, _ := evt.Data["tool"].(string)
		switch name {
		case "mcp.testsrv.greet", "mcp_testsrv_greet":
			greetCalls++
		case "mcp.testsrv.reverse", "mcp_testsrv_reverse":
			reverseCalls++
		}
	}
	t.Logf("MCP tool calls: greet=%d reverse=%d", greetCalls, reverseCalls)
	if greetCalls < 1 {
		t.Errorf("MCP: agent did not call mcp.testsrv.greet")
	}
	if reverseCalls < 1 {
		t.Errorf("MCP: agent did not call mcp.testsrv.reverse")
	}

	// Verify the agent output captured the tool results.
	for _, evt := range events {
		if evt.Type != store.EventNodeFinished || evt.NodeID != "mcp_caller" || evt.Data == nil {
			continue
		}
		out, _ := evt.Data["output"].(map[string]interface{})
		if out == nil {
			continue
		}
		greeting, _ := out["greeting"].(string)
		reversed, _ := out["reversed"].(string)
		toolsUsed, _ := out["tools_used"].([]interface{})
		t.Logf("greeting = %q", greeting)
		t.Logf("reversed = %q", reversed)
		t.Logf("tools_used = %v", toolsUsed)
		if !strings.Contains(greeting, "Iterion") {
			t.Errorf("greeting missing 'Iterion': %q", greeting)
		}
		if reversed != "setatS" {
			t.Errorf("reversed != \"setatS\": got %q", reversed)
		}
		break
	}

	logRunRecap(t, events)
}

// ---------------------------------------------------------------------------
// Live E2E test — claw long-context
// ---------------------------------------------------------------------------

// TestLive_Lite_ClawLongContext sends a multi-thousand-token prompt through
// claw + Anthropic Haiku 4.5 (200k window) and verifies that:
//
//  1. The run finishes successfully (no preflight rejection, no encoder
//     OOM, no stream truncation).
//  2. The agent's `summary` field is non-empty.
//  3. The agent's `marker` field exactly equals the SHA-1 sentinel we
//     planted at the top of the input — proving the input reached the
//     model whole, not truncated mid-prompt.
//  4. EventLLMStepFinished reports input_tokens > a high threshold,
//     confirming the request really carried the bulk of the text.
//
// Requires ANTHROPIC_API_KEY.
func TestLive_Lite_ClawLongContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "ANTHROPIC_API_KEY")

	wf := compileFixture(t, "claw_long_context.iter")

	workspaceDir, err := os.MkdirTemp("", "iterion-claw-long-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	storeDir := filepath.Join(workspaceDir, ".iterion")
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("Failed to create store: %v", storeErr)
	}
	runID := "live-claw-long-context"

	executor := newLiveExecutor(wf, s, runID, workspaceDir)
	defer executor.Close()
	executor.SetVars(map[string]interface{}{
		"workspace_dir": workspaceDir,
	})

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	// Build the long input. The marker is a fixed SHA-1-like sentinel at
	// the top so the model can echo it back to prove the prompt arrived
	// whole. The body is a repeated paragraph; ~5000 repeats yields
	// roughly 25k–35k tokens depending on the tokenizer.
	const marker = "MARKER_SHA1=4af3c2b8a7d10e9f6512cd84e0b3f7a9d1e6b0c2"
	paragraph := "The Holocene is a geological epoch that began approximately 11,700 years before the present. It follows the Last Glacial Period, characterized by periodic glaciations and the eventual retreat of the ice sheets that covered large portions of the Northern Hemisphere. During the Holocene, human civilizations emerged, agricultural practices spread, and complex societies developed across multiple continents. "
	var sb strings.Builder
	sb.WriteString(marker)
	sb.WriteString("\n\nLong corpus follows; please ignore content and just produce a one-paragraph summary.\n\n")
	for i := 0; i < 800; i++ {
		fmt.Fprintf(&sb, "[para %d] %s\n\n", i+1, paragraph)
	}
	longText := sb.String()
	t.Logf("Long input: %d bytes (~%d tokens estimate)", len(longText), len(longText)/4)

	eng := runtime.New(wf, s, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	start := time.Now()
	if runErr := eng.Run(ctx, runID, map[string]interface{}{
		"text": longText,
	}); runErr != nil {
		t.Fatalf("Run error: %v", runErr)
	}
	t.Logf("Run completed in %s", time.Since(start).Round(time.Second))

	r, _ := s.LoadRun(runID)
	if r.Status != store.RunStatusFinished {
		t.Fatalf("Expected run status 'finished', got %q", r.Status)
	}

	events, evtErr := s.LoadEvents(runID)
	if evtErr != nil {
		t.Fatalf("LoadEvents: %v", evtErr)
	}

	// Output sanity.
	for _, evt := range events {
		if evt.Type != store.EventNodeFinished || evt.NodeID != "summarizer" || evt.Data == nil {
			continue
		}
		out, _ := evt.Data["output"].(map[string]interface{})
		if out == nil {
			continue
		}
		summary, _ := out["summary"].(string)
		gotMarker, _ := out["marker"].(string)
		t.Logf("summary (%d chars): %s", len(summary), summary)
		t.Logf("marker echo: %q", gotMarker)
		if strings.TrimSpace(summary) == "" {
			t.Error("LONG CONTEXT: empty summary")
		}
		// The unique SHA portion is what proves the model saw the top of
		// the input. Some models echo only the value, not the "KEY=" prefix.
		if !strings.Contains(gotMarker, "4af3c2b8a7d10e9f6512cd84e0b3f7a9d1e6b0c2") {
			t.Errorf("LONG CONTEXT: marker SHA not echoed — model may have truncated input. got=%q", gotMarker)
		}
		break
	}

	// Token-count assertion.
	maxInputTokens := 0
	for _, evt := range events {
		if evt.Type != store.EventLLMStepFinished || evt.NodeID != "summarizer" || evt.Data == nil {
			continue
		}
		switch v := evt.Data["input_tokens"].(type) {
		case int:
			if v > maxInputTokens {
				maxInputTokens = v
			}
		case int64:
			if int(v) > maxInputTokens {
				maxInputTokens = int(v)
			}
		case float64:
			if int(v) > maxInputTokens {
				maxInputTokens = int(v)
			}
		}
	}
	t.Logf("Max input_tokens reported: %d", maxInputTokens)
	const minExpectedTokens = 20000
	if maxInputTokens < minExpectedTokens {
		t.Errorf("LONG CONTEXT: input_tokens=%d is below expected threshold %d — prompt may have been truncated upstream", maxInputTokens, minExpectedTokens)
	} else {
		t.Logf("LONG CONTEXT VALIDATED: claw shipped %d input tokens to Anthropic", maxInputTokens)
	}

	logRunRecap(t, events)
}

// requireBinaryInPath skips the test if the named binary is not in PATH.
func requireBinaryInPath(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not in PATH — skipping", name)
	}
}

// requireEnv skips the test if the named environment variable is not set.
func requireEnv(t *testing.T, name string) {
	t.Helper()
	if os.Getenv(name) == "" {
		t.Skipf("%s not set — skipping live delegation test", name)
	}
}

// ---------------------------------------------------------------------------
// Run recapitulation helper
// ---------------------------------------------------------------------------

// logRunRecap prints a detailed recapitulation of all workflow steps and
// LLM discussions from the event log.
func logRunRecap(t *testing.T, events []*store.Event) {
	t.Helper()

	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString("╔══════════════════════════════════════════════════════════════════╗\n")
	sb.WriteString("║                    RUN RECAPITULATION                           ║\n")
	sb.WriteString("╚══════════════════════════════════════════════════════════════════╝\n\n")

	stepNum := 0
	for _, evt := range events {
		switch evt.Type {
		case store.EventRunStarted:
			sb.WriteString(fmt.Sprintf("▶ RUN STARTED  [%s]\n\n", evt.Timestamp.Format(time.RFC3339)))

		case store.EventNodeStarted:
			stepNum++
			kind := ""
			if evt.Data != nil {
				if k, ok := evt.Data["kind"].(string); ok {
					kind = k
				}
			}
			branch := ""
			if evt.BranchID != "" {
				branch = fmt.Sprintf("  [branch: %s]", evt.BranchID)
			}
			sb.WriteString(fmt.Sprintf("── Step %d: %s (%s)%s ──\n", stepNum, evt.NodeID, kind, branch))

			// Round-robin metadata.
			if evt.Data != nil {
				if idx, ok := evt.Data["round_robin_index"]; ok {
					sb.WriteString(fmt.Sprintf("   Round-robin index: %v → %v\n", idx, evt.Data["selected_target"]))
				}
			}

		case store.EventLLMPrompt:
			if evt.Data == nil {
				continue
			}
			sb.WriteString(fmt.Sprintf("   📤 LLM PROMPT [node: %s]\n", evt.NodeID))
			if sys, ok := evt.Data["system_prompt"].(string); ok && sys != "" {
				sb.WriteString(fmt.Sprintf("   ┌─ System Prompt (%d chars):\n", len(sys)))
				sb.WriteString(indentTruncate(sys, 80, 500))
				sb.WriteString("\n")
			}
			if usr, ok := evt.Data["user_message"].(string); ok && usr != "" {
				sb.WriteString(fmt.Sprintf("   ┌─ User Message (%d chars):\n", len(usr)))
				sb.WriteString(indentTruncate(usr, 80, 1000))
				sb.WriteString("\n")
			}

		case store.EventLLMStepFinished:
			if evt.Data == nil {
				continue
			}
			sb.WriteString(fmt.Sprintf("   📥 LLM RESPONSE [node: %s]\n", evt.NodeID))
			if resp, ok := evt.Data["response_text"].(string); ok && resp != "" {
				sb.WriteString(fmt.Sprintf("   ┌─ Response (%d chars):\n", len(resp)))
				sb.WriteString(indentTruncate(resp, 80, 1000))
				sb.WriteString("\n")
			}
			tokens := ""
			if in, ok := evt.Data["input_tokens"]; ok {
				tokens += fmt.Sprintf("in=%v ", in)
			}
			if out, ok := evt.Data["output_tokens"]; ok {
				tokens += fmt.Sprintf("out=%v ", out)
			}
			if tokens != "" {
				sb.WriteString(fmt.Sprintf("   Tokens: %s\n", tokens))
			}

		case store.EventNodeFinished:
			if evt.Data == nil {
				continue
			}
			backendTag := ""
			if d, ok := evt.Data["_backend"].(string); ok {
				backendTag = fmt.Sprintf(" [backend: %s]", d)
			}
			// Show node output summary.
			if output, ok := evt.Data["output"]; ok {
				outJSON, _ := json.Marshal(output)
				outStr := string(outJSON)
				if len(outStr) > 500 {
					outStr = outStr[:500] + "..."
				}
				sb.WriteString(fmt.Sprintf("   ✅ FINISHED: %s%s\n", evt.NodeID, backendTag))
				sb.WriteString(fmt.Sprintf("   Output: %s\n\n", outStr))
			}

		case store.EventEdgeSelected:
			if evt.Data == nil {
				continue
			}
			from, _ := evt.Data["from"].(string)
			to, _ := evt.Data["to"].(string)
			info := fmt.Sprintf("   → Edge: %s → %s", from, to)
			if cond, ok := evt.Data["condition"].(string); ok {
				negated, _ := evt.Data["negated"].(bool)
				if negated {
					info += fmt.Sprintf(" (when NOT %s)", cond)
				} else {
					info += fmt.Sprintf(" (when %s)", cond)
				}
			}
			if loop, ok := evt.Data["loop"].(string); ok {
				iter, _ := evt.Data["iteration"]
				info += fmt.Sprintf(" [loop: %s, iteration: %v]", loop, iter)
			}
			sb.WriteString(info + "\n")

		case store.EventArtifactWritten:
			if evt.Data == nil {
				continue
			}
			sb.WriteString(fmt.Sprintf("   📦 Artifact: %s (publish: %v, version: %v)\n",
				evt.NodeID, evt.Data["publish"], evt.Data["version"]))

		case store.EventBranchStarted:
			sb.WriteString(fmt.Sprintf("   🔀 Branch started: %s → %s\n",
				evt.BranchID, evt.NodeID))

		case store.EventJoinReady:
			if evt.Data != nil {
				sb.WriteString(fmt.Sprintf("   🔗 Join ready: %s (required: %v)\n",
					evt.NodeID, evt.Data["required"]))
			}

		case store.EventBudgetWarning:
			if evt.Data != nil {
				sb.WriteString(fmt.Sprintf("   ⚠️  Budget warning: %v (used: %v / limit: %v)\n",
					evt.Data["dimension"], evt.Data["used"], evt.Data["limit"]))
			}

		case store.EventRunFinished:
			sb.WriteString(fmt.Sprintf("\n✅ RUN FINISHED  [%s]\n", evt.Timestamp.Format(time.RFC3339)))

		case store.EventRunFailed:
			if evt.Data != nil {
				sb.WriteString(fmt.Sprintf("\n❌ RUN FAILED  [%s]\n   %v: %v\n",
					evt.Timestamp.Format(time.RFC3339), evt.Data["code"], evt.Data["error"]))
			}
		}
	}

	sb.WriteString("\n════════════════════════════════════════════════════════════════════\n")
	t.Log(sb.String())
}

// indentTruncate indents each line of text and truncates per-line and total.
func indentTruncate(text string, maxLineLen, maxTotal int) string {
	var sb strings.Builder
	lines := strings.Split(text, "\n")
	total := 0
	for _, line := range lines {
		if total > maxTotal {
			sb.WriteString("   │ ... (truncated)\n")
			break
		}
		if len(line) > maxLineLen {
			line = line[:maxLineLen] + "..."
		}
		sb.WriteString("   │ " + line + "\n")
		total += len(line)
	}
	return sb.String()
}
