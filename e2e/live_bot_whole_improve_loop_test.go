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
	// ADR-057: improvement_prompt is THE AXIS. Point the sweep at the planted
	// logic bug so enumerate produces a concrete, targetable work-item.
	axis := "Fix logic/correctness bugs in the Go source at the repository root (e.g. off-by-one loop bounds). One work-item per buggy function."
	executor.SetVars(map[string]interface{}{
		"workspace_dir":      workspaceDir,
		"improvement_prompt": axis,
		"scope_notes":        "Two files at the repo root; one has an off-by-one in a loop bound.",
	})

	eng := runtime.New(wf, s, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	inputs := map[string]interface{}{
		"workspace_dir":      workspaceDir,
		"improvement_prompt": axis,
		"scope_notes":        "Two files at the repo root; one has an off-by-one in a loop bound.",
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

	// ADR-057 axis sweep: count the sweep drivers (transform + a reviewer)
	// rather than the retired streak_check. A small axis may only alternate one
	// family, so assert the sweep ran (a transform + a review + a next_item),
	// not that both families fired.
	finished := eventNodeIDs(events, store.EventNodeFinished)
	claudeReviewed, gptReviewed := 0, 0
	transforms, sweeps := 0, 0
	for _, id := range finished {
		switch id {
		case "reviewer_claude":
			claudeReviewed++
		case "reviewer_gpt":
			gptReviewed++
		case "transform":
			transforms++
		case "next_item":
			sweeps++
		}
	}
	t.Logf("Counts: reviewer_claude=%d reviewer_gpt=%d transform=%d next_item=%d",
		claudeReviewed, gptReviewed, transforms, sweeps)
	if claudeReviewed+gptReviewed == 0 {
		t.Errorf("expected at least one reviewer to fire (claude=%d gpt=%d)", claudeReviewed, gptReviewed)
	}
	if transforms == 0 {
		t.Errorf("expected the sweep to transform at least one work-item, got 0")
	}
	if sweeps < 2 {
		t.Errorf("expected ≥2 next_item passes (the sweep advanced), got %d", sweeps)
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
	// ADR-057: the sweep needs an AXIS. Point it at the fixture's planted
	// production-blocking defects so enumerate produces concrete work-items.
	axis := "Fix production-blocking correctness, concurrency, and security defects across queue/, worker/, auth/, storage/, config/, and main.go (race on a shared map, goroutine leak, timing-attack auth compare, path traversal, unbounded dequeue on close). One work-item per defect site. Skip stylistic nits."
	scopeNotes := "A curated Go fixture with a mix of clean modules and ones carrying the defects named in the axis."
	executor.SetVars(map[string]interface{}{
		"improvement_prompt": axis,
		"scope_notes":        scopeNotes,
	})

	eng := runtime.New(wf, s, executor, runtime.WithWorkDir(workspaceDir), runtime.WithLogger(teeLogger))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	inputs := map[string]interface{}{
		"improvement_prompt": axis,
		"scope_notes":        scopeNotes,
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
