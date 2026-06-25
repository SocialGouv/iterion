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

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLive_VibeReviewAlternating runs the alternating review/fix loop
// against a real Claude + GPT pairing. Workspace is seeded with two
// .go files: one clean, one with a deliberate bug (off-by-one). The
// bot's loop should either reach cross-family approval (streak stop) OR
// exhaust the loop budget — both are graceful terminations.
//
// Requires:
//   - `claude` CLI installed.
//   - OPENAI_API_KEY for the GPT branch (claw).
func TestLive_VibeReviewAlternating(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireBinaryInPath(t, "docker")
	requireOpenAI(t)

	wf := compileFixture(t, "whole-improve-loop/main.bot")

	workspaceDir, err := os.MkdirTemp("", "iterion-whole-improve-loop-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)
	seedGitRepo(t, workspaceDir)

	// One clean file + one with a subtle bug. The bug is real but small,
	// so reviewers won't trivially refuse to converge.
	clean := `package fixture

// Add returns the sum of a and b.
func Add(a, b int) int { return a + b }
`
	buggy := `package fixture

// Multiply returns a multiplied by b. Has an off-by-one in the loop bound:
// the result is (a-1)*b instead of a*b for positive a.
func Multiply(a, b int) int {
	result := 0
	for i := 0; i < a-1; i++ {
		result += b
	}
	return result
}
`
	if err := os.WriteFile(filepath.Join(workspaceDir, "add.go"), []byte(clean), 0o644); err != nil {
		t.Fatalf("write add.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "multiply.go"), []byte(buggy), 0o644); err != nil {
		t.Fatalf("write multiply.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "go.mod"), []byte("module iterion-live-fixture\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	runCmd(t, workspaceDir, "git", "add", "add.go", "multiply.go", "go.mod")
	runCmd(t, workspaceDir, "git", "commit", "-m", "chore: seed code under review")

	storeDir := resolveLiveStoreDir(t, workspaceDir)
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runID := "live-whole-improve-loop"

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}
	executor := newLiveExecutor(t, wf, s, runID, workspaceDir)
	defer executor.Close()
	executor.SetVars(map[string]interface{}{
		"workspace_dir": workspaceDir,
		"scope_notes":   "Review every .go file at the repository root for correctness, focus on logic bugs.",
	})

	eng := runtime.New(wf, s, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	inputs := map[string]interface{}{
		"workspace_dir": workspaceDir,
		"scope_notes":   "Review every .go file at the repository root for correctness, focus on logic bugs.",
	}

	t.Log("Starting whole_improve_loop live run…")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run finished in %s", elapsed.Round(time.Second))

	acceptable, reason := liveRunResultAcceptable(runErr)
	if !acceptable {
		t.Fatalf("unacceptable run error: %v", runErr)
	}
	t.Logf("Run result: %s", reason)

	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}

	finished := eventNodeIDs(events, store.EventNodeFinished)
	claudeReviewed, gptReviewed := 0, 0
	streakChecks := 0
	for _, id := range finished {
		switch id {
		case "reviewer_claude":
			claudeReviewed++
		case "reviewer_gpt":
			gptReviewed++
		case "streak_check":
			streakChecks++
		}
	}
	t.Logf("Counts: reviewer_claude=%d reviewer_gpt=%d streak_check=%d",
		claudeReviewed, gptReviewed, streakChecks)
	if claudeReviewed == 0 || gptReviewed == 0 {
		t.Errorf("expected both reviewers to fire at least once (claude=%d gpt=%d)",
			claudeReviewed, gptReviewed)
	}
	if streakChecks < 2 {
		t.Errorf("expected ≥2 streak_check invocations (loop ran), got %d", streakChecks)
	}

	writeLiveTestReport(t, runID, workspaceDir, storeDir, s, events)
	assessQualityRaw(t, "whole-improve-loop", "Willy", "Review seeded Go files and fix the planted off-by-one bug", runID, workspaceDir, storeDir, s, events, time.Since(start), reason, gitArtifactEvidence(t, workspaceDir))
}

// TestLive_VibeReviewAlternating_Real runs the review/fix loop against
// e2e/fixtures/review-pr-mix — a curated codebase with a deliberate
// mix of clean modules and ones with production-blocking issues
// (race condition on a shared map, goroutine leak, timing-attack
// auth comparison, path-traversal in storage, unbounded queue dequeue
// on close). Observes which issues both reviewers catch, what they
// miss, and how the fixers patch them.
//
// Expected duration: 30-90 min, $10-30.
func TestLive_VibeReviewAlternating_Real(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireBinaryInPath(t, "docker")
	requireOpenAI(t)

	wf := compileFixture(t, "whole-improve-loop/main.bot")
	workspaceDir := seedFromFixture(t, "review-pr-mix")

	storeDir := resolveLiveStoreDir(t, workspaceDir)
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runID := uniqueRunID("live-whole-improve-loop-real")

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}
	teeLogger := prepareLiveRunLog(t, storeDir, runID)
	executor := newLiveExecutor(t, wf, s, runID, workspaceDir, withLiveLogger(teeLogger))
	defer executor.Close()
	// Don't override workspace_dir — same rationale as
	// TestLive_FeatureDev_Real. Let ${PROJECT_DIR} resolve to
	// /workspace inside the sandbox.
	scopeNotes := "Review every Go file across queue/, worker/, auth/, storage/, config/, and main.go. Focus on production-blocking correctness, concurrency, and security issues. Skip stylistic nits."
	executor.SetVars(map[string]interface{}{
		"scope_notes": scopeNotes,
	})

	eng := runtime.New(wf, s, executor, runtime.WithWorkDir(workspaceDir), runtime.WithLogger(teeLogger))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	inputs := map[string]interface{}{
		"scope_notes": scopeNotes,
	}

	commitsBefore := workspaceCommitCount(t, workspaceDir)
	t.Log("Starting whole_improve_loop (real fixture) live run…")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	t.Logf("Run finished in %s", time.Since(start).Round(time.Second))

	acceptable, reason := liveRunResultAcceptableReal(runErr)
	if !acceptable {
		captureSandboxDiagnostics(t, runID)
		t.Fatalf("unacceptable run error: %v", runErr)
	}
	t.Logf("Run result: %s", reason)

	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	requireWorkspaceCommitGrowth(t, workspaceDir, commitsBefore)
	cmd := exec.Command("git", "-C", workspaceDir, "diff", "--stat", "HEAD")
	out, _ := cmd.CombinedOutput()
	if len(strings.TrimSpace(string(out))) > 0 {
		t.Logf("Uncommitted fixer changes (working tree):\n%s", string(out))
	}

	writeLiveTestReport(t, runID, workspaceDir, storeDir, s, events)
	assessQualityRaw(t, "whole-improve-loop", "Willy", "Review a curated multi-issue Go codebase and fix production-blocking bugs", runID, workspaceDir, storeDir, s, events, time.Since(start), reason, gitArtifactEvidence(t, workspaceDir))
}
