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
	iterlog "github.com/SocialGouv/iterion/log"
	"github.com/SocialGouv/iterion/mcp"
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
func TestLive_DualModel_PlanImplementReview(t *testing.T) {
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

	reg := model.NewRegistry()
	logger := iterlog.New(iterlog.LevelDebug, os.Stderr)
	hooks := model.NewStoreEventHooks(s, runID, logger)

	execOpts := []model.GoaiExecutorOption{
		model.WithDelegateRegistry(delegate.DefaultRegistry()),
		model.WithWorkDir(workspaceDir),
		model.WithEventHooks(hooks),
	}

	executor := model.NewGoaiExecutor(reg, wf, execOpts...)
	defer executor.Close()

	taskDescription := "An interactive astronomical observatory and night sky simulator in a single index.html file. " +
		"Use vanilla HTML, CSS, and JavaScript (no frameworks or CDNs). " +
		"The app renders an astronomically accurate SVG star map with real constellation data, " +
		"advanced interactivity, and educational features that make it a compelling learning tool."

	acceptanceCriteria := "=== Layer 1 - Foundation ===\n" +
		"1. Single index.html file with all CSS and JS embedded, no external dependencies\n" +
		"2. SVG-based star map on a realistic dark gradient background simulating the night sky\n" +
		"3. At least 12 constellations with accurate relative star positions and connecting lines\n" +
		"4. Stars rendered as circles with 4 distinct size classes based on apparent magnitude (mag 1-2, 2-3, 3-4, 4-5)\n" +
		"5. Click a constellation to show an info panel with: name, main stars, and a mythology story\n" +
		"\n=== Layer 2 - Interactivity ===\n" +
		"6. Season selector (4 buttons: Spring, Summer, Autumn, Winter) that smoothly rotates the visible sky with CSS transition (rotation animation over 0.5s)\n" +
		"7. Search input that filters constellations by name with real-time highlighting (matching constellation glows)\n" +
		"8. Hover over any star to show a tooltip with the star's name (at least 20 named stars like Sirius, Betelgeuse, Polaris, etc.)\n" +
		"9. A draggable/pannable star map: click-and-drag to pan the view, mouse wheel to zoom in/out\n" +
		"10. Responsive layout that works on both desktop (side panel) and mobile (bottom sheet panel) with a CSS media query breakpoint at 768px\n" +
		"\n=== Layer 3 - Visual Polish ===\n" +
		"11. Stars twinkle with a subtle CSS animation (opacity oscillation, each star with a slightly different animation delay)\n" +
		"12. Constellation lines fade in with a drawing animation when a constellation is selected\n" +
		"13. A magnitude slider (range input) that filters visible stars by brightness threshold in real-time\n" +
		"14. The Milky Way rendered as a semi-transparent gradient band across the sky\n" +
		"15. A compass rose indicator showing N/S/E/W orientation that updates as the map is panned\n" +
		"\n=== Layer 4 - Educational Depth ===\n" +
		"16. Each constellation includes mythology from at least 3 different cultures (Greek, Chinese, and one indigenous culture: Aboriginal, Inuit, Polynesian, or other)\n" +
		"17. A constellation quiz mode: the app highlights stars and the user must identify the constellation from 4 multiple-choice options, with score tracking\n" +
		"18. A time-of-night slider (8pm to 4am) that adjusts star brightness and sky gradient to simulate the darkening sky\n" +
		"19. Keyboard accessibility: arrow keys to pan, +/- to zoom, Escape to close panels, Tab to navigate constellations\n" +
		"20. A 'tonight sky' button that calculates the approximate visible constellations based on the current month (using JavaScript Date)"

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
			delegate := ""
			if d, ok := evt.Data["_delegate"].(string); ok {
				delegate = fmt.Sprintf(" [delegate: %s]", d)
			}
			// Show node output summary.
			if output, ok := evt.Data["output"]; ok {
				outJSON, _ := json.Marshal(output)
				outStr := string(outJSON)
				if len(outStr) > 500 {
					outStr = outStr[:500] + "..."
				}
				sb.WriteString(fmt.Sprintf("   ✅ FINISHED: %s%s\n", evt.NodeID, delegate))
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
