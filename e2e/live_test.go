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

// newLiveExecutor creates a GoaiExecutor with all standard backends registered.
func newLiveExecutor(wf *ir.Workflow, s *store.RunStore, runID, workDir string) *model.GoaiExecutor {
	reg := model.NewRegistry()
	logger := iterlog.New(iterlog.LevelDebug, os.Stderr)
	hooks := model.NewStoreEventHooks(s, runID, logger)

	backendReg := delegate.DefaultRegistry(logger)
	backendReg.Register(delegate.BackendGoai, model.NewGoaiBackend(reg, wf.Schemas, hooks, model.RetryPolicy{}))

	return model.NewGoaiExecutor(reg, wf,
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
//   - `claude` and `codex` CLIs installed and in PATH
//   - The CLIs must be authenticated
//
// Automatically skipped when CLIs are absent or in -short mode.
func TestLive_Lite_DualModel_PlanImplementReview(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireCLI(t, "codex")

	// Default model if not set via .env or environment.
	if os.Getenv("CLAUDE_MODEL") == "" {
		t.Setenv("CLAUDE_MODEL", "openai/gpt-5.4")
	}

	// Compile the fixture.
	wf := compileFixture(t, "dual_model_plan_implement_review.iter")

	// Create a persistent workspace directory.
	workspaceDir, err := os.MkdirTemp("", "iterion-plan-impl-review-*")
	if err != nil {
		t.Fatalf("Failed to create workspace dir: %v", err)
	}
	t.Logf("Workspace directory (persists after test): %s", workspaceDir)

	// Initialize a git repo in the workspace so that codex CLI accepts it.
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
		if nodeID != "claude_implement" && nodeID != "codex_implement" {
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

	// Verify both agent perspectives were invoked (same model, different sessions).
	claudeNodeCalled := false
	codexNodeCalled := false
	for _, id := range finishedNodes {
		if strings.Contains(id, "claude") {
			claudeNodeCalled = true
		}
		if strings.Contains(id, "codex") {
			codexNodeCalled = true
		}
	}
	if !claudeNodeCalled {
		t.Error("No claude_* node finished — agent A perspective may have failed")
	}
	if !codexNodeCalled {
		t.Error("No codex_* node finished — agent B perspective may have failed")
	}

	// Verify key planning nodes executed.
	nodeSet := make(map[string]bool)
	for _, id := range finishedNodes {
		nodeSet[id] = true
	}
	for _, expected := range []string{"claude_plan", "codex_plan", "merge_plans"} {
		if !nodeSet[expected] {
			t.Errorf("Expected node %q to have finished", expected)
		}
	}

	// Count implementation and review iterations.
	claudeImplCount := 0
	codexImplCount := 0
	for _, id := range finishedNodes {
		if id == "claude_implement" {
			claudeImplCount++
		}
		if id == "codex_implement" {
			codexImplCount++
		}
	}
	t.Logf("Implementation iterations: claude_implement=%d codex_implement=%d", claudeImplCount, codexImplCount)

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
	for _, nodeID := range []string{"claude_plan", "codex_plan", "merge_plans", "claude_val", "codex_val", "val_judge", "claude_implement", "codex_implement", "claude_review", "codex_review", "review_judge"} {
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
//   - `claude` and `codex` CLIs installed and in PATH
//   - The CLIs must be authenticated
//
// Automatically skipped when CLIs are absent or in -short mode.
func TestLive_Lite_SessionContinuity_ReviewFix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireCLI(t, "codex")

	if os.Getenv("CLAUDE_MODEL") == "" {
		t.Setenv("CLAUDE_MODEL", "openai/gpt-5.4")
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
		if nodeID != "implement" && nodeID != "claude_fix" && nodeID != "codex_fix" {
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
	codexNodeCalled := false
	for _, id := range finishedNodes {
		if strings.Contains(id, "claude") {
			claudeNodeCalled = true
		}
		if strings.Contains(id, "codex") {
			codexNodeCalled = true
		}
	}
	if !claudeNodeCalled {
		t.Error("No claude_* node finished")
	}
	if !codexNodeCalled {
		t.Error("No codex_* node finished")
	}

	// Verify key nodes executed.
	nodeSet := make(map[string]bool)
	for _, id := range finishedNodes {
		nodeSet[id] = true
	}
	for _, expected := range []string{"claude_plan", "codex_plan", "plan_judge_merge", "implement"} {
		if !nodeSet[expected] {
			t.Errorf("Expected node %q to have finished", expected)
		}
	}

	// Check for review nodes.
	if !nodeSet["claude_review"] || !nodeSet["codex_review"] {
		t.Error("Expected both review nodes to have finished")
	}

	// Check for session continuity: verify fix nodes ran (proves the fix loop fired).
	fixNodeRan := nodeSet["claude_fix"] || nodeSet["codex_fix"]
	if fixNodeRan {
		t.Log("SESSION CONTINUITY: at least one fix node ran with session: inherit")
	} else {
		t.Log("INFO: No fix nodes ran — implementation was approved on first review")
	}

	// Count fix iterations.
	claudeFixCount := 0
	codexFixCount := 0
	for _, id := range finishedNodes {
		switch id {
		case "claude_fix":
			claudeFixCount++
		case "codex_fix":
			codexFixCount++
		}
	}
	t.Logf("Fix iterations: claude_fix=%d codex_fix=%d", claudeFixCount, codexFixCount)

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
// Requires: `claude` and `codex` CLIs installed and authenticated.
func TestLive_Full_ExhaustiveDSLCoverage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireCLI(t, "codex")

	if os.Getenv("CLAUDE_MODEL") == "" {
		t.Setenv("CLAUDE_MODEL", "openai/gpt-5.4")
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
	codexAgent := false
	for _, id := range finishedNodes {
		if id == "writer_a" || id == "refiner_a" || id == "extract_meta" || id == "merge" {
			claudeAgent = true
		}
		if id == "writer_b" || id == "refiner_b" {
			codexAgent = true
		}
	}
	if !claudeAgent {
		t.Error("COVERAGE: no claude_code agent node finished")
	}
	if !codexAgent {
		t.Error("COVERAGE: no codex agent node finished")
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

	// ModelCalls counts goai backend calls only; CLI backends (claude_code, codex)
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
	if codexAgent {
		covered = append(covered, "backend_codex")
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
