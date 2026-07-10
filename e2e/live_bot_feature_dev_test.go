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

// TestLive_FeatureDev runs the feature_dev bot against a real LLM.
// v2 (ADR-058 minimal-framing): ONE campaign agent ships the feature
// slice by slice committing in stride, then the deterministic verify
// gate re-checks the tree — so success means: at least one new commit
// landed in the workspace's git history beyond the seed commit.
//
// Requires:
//   - `claude` CLI installed (and OAuth-authenticated OR ZAI_API_KEY in env).
//
// The workspace dir is NOT removed after the test so the user can
// inspect the resulting code + report.md + metrics.json. The
// workspace also gets symlinked into e2e/.workspaces/<test-name>/.
func TestLive_FeatureDev(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireBinaryInPath(t, "docker")

	wf := compileFixture(t, "feature-dev/main.bot")

	workspaceDir, err := os.MkdirTemp("", "iterion-feature-dev-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)
	seedGitRepo(t, workspaceDir)

	storeDir := resolveLiveStoreDir(t, workspaceDir)
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runID := "live-feature-dev"

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}
	executor := newLiveExecutor(t, wf, s, runID, workspaceDir)
	defer executor.Close()
	executor.SetVars(map[string]any{
		"workspace_dir":  workspaceDir,
		"feature_prompt": "Add a function `Answer() int` returning 42 in answer.go at the repository root, plus a Go test in answer_test.go that asserts the return value. The repo currently has no Go files; create a minimal go.mod (`module iterion-live-fixture`, go 1.25) alongside.",
	})

	eng := runtime.New(wf, s, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	inputs := map[string]any{
		"feature_prompt": "Add a function `Answer() int` returning 42 in answer.go at the repository root, plus a Go test in answer_test.go that asserts the return value. The repo currently has no Go files; create a minimal go.mod (`module iterion-live-fixture`, go 1.25) alongside.",
		"workspace_dir":  workspaceDir,
	}

	t.Log("Starting feature_dev live run…")
	start := time.Now()
	runErr := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run finished in %s", elapsed.Round(time.Second))

	acceptable, reason := liveRunResultAcceptable(runErr)
	if !acceptable {
		t.Fatalf("unacceptable run error: %v", runErr)
	}
	t.Logf("Run result: %s", reason)

	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	t.Logf("Status: %s", r.Status)

	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}

	finished := eventNodeIDs(events, store.EventNodeFinished)
	finishedSet := map[string]bool{}
	for _, id := range finished {
		finishedSet[id] = true
	}
	for _, phase := range []string{"campaign", "verify_build", "verify_run", "gate"} {
		if !finishedSet[phase] {
			t.Errorf("v2 node %q never finished — the campaign→verify→gate pass did not complete", phase)
		}
	}

	// The acceptance criterion: at least one new commit beyond the seed.
	cmd := exec.Command("git", "-C", workspaceDir, "rev-list", "--count", "HEAD")
	out, err := cmd.CombinedOutput()
	commitCount := strings.TrimSpace(string(out))
	if err != nil {
		t.Errorf("git rev-list failed: %v\n%s", err, out)
	} else if commitCount == "1" {
		t.Errorf("expected at least 2 commits (seed + one feature commit), got %s — commit_changes likely never landed", commitCount)
	} else {
		t.Logf("Commits: %s (seed + features)", commitCount)
	}

	// Probe for the expected artefact (answer.go) — informational only;
	// the bot may legitimately put it elsewhere in a worktree run.
	if _, err := os.Stat(filepath.Join(workspaceDir, "answer.go")); err == nil {
		t.Logf("answer.go present at workspace root")
	} else {
		t.Logf("answer.go not at workspace root (worktree run may have committed elsewhere — see report.md)")
	}

	writeLiveTestReport(t, runID, workspaceDir, storeDir, s, events)
	assessQualityRaw(t, "feature-dev", "Featurly", "Add Answer() int returning 42 with a Go test", runID, workspaceDir, storeDir, s, events, time.Since(start), reason, gitArtifactEvidence(t, workspaceDir))
}

// TestLive_FeatureDev_Real runs feature_dev against a real-world
// starting point: a small Go HTTP service under
// e2e/fixtures/feature-dev-go-service. The feature prompt is non-
// trivial — adds a new endpoint with validation, error handling, and
// idempotency. Observes plan quality, implementation correctness,
// reviewer scrutiny, and final commit semantic.
//
// Requires: claude CLI + OPENAI_API_KEY.
// Expected duration: 20-60 min, $5-15.
func TestLive_FeatureDev_Real(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireBinaryInPath(t, "docker")

	wf := compileFixture(t, "feature-dev/main.bot")
	workspaceDir := seedFromFixture(t, "feature-dev-go-service")

	storeDir := resolveLiveStoreDir(t, workspaceDir)
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	runID := uniqueRunID("live-feature-dev-real")

	if err := mcp.PrepareWorkflow(wf, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}
	teeLogger := prepareLiveRunLog(t, storeDir, runID)
	executor := newLiveExecutor(t, wf, s, runID, workspaceDir, withLiveLogger(teeLogger))
	defer executor.Close()
	featurePrompt := "Add a `POST /users/{id}/posts` endpoint that creates a new Post for the given user. " +
		"Requirements: title is required and ≤200 chars, body is required, return 404 if the user " +
		"does not exist, return 400 on validation errors with a JSON {\"error\":\"...\"} shape consistent " +
		"with the existing handlers. Support idempotency via the `Idempotency-Key` header — when the same " +
		"key + user_id is seen twice within 5 minutes, return the original response without creating a " +
		"duplicate. Add a method to internal/store that creates the post (preserving the existing " +
		"mutex-guarded pattern). Add table-driven tests covering: happy path, missing title, missing body, " +
		"oversize title (>200), nonexistent user, replay with the same idempotency key, and a different " +
		"key on the same user (should create two posts)."
	// Do NOT set workspace_dir here. The bot's `vars.workspace_dir`
	// defaults to ${PROJECT_DIR} which iterion remaps to /workspace
	// (the container bind-mount) once the sandbox is up. Overriding
	// would replace it with the host tempdir path, which doesn't
	// exist inside the container and every shell tool would fail.
	executor.SetVars(map[string]any{
		"feature_prompt": featurePrompt,
	})

	eng := runtime.New(wf, s, executor, runtime.WithWorkDir(workspaceDir), runtime.WithLogger(teeLogger))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	inputs := map[string]any{
		"feature_prompt": featurePrompt,
	}

	commitsBefore := workspaceCommitCount(t, workspaceDir)
	t.Log("Starting feature_dev (real fixture) live run…")
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
	cmd := exec.Command("git", "-C", workspaceDir, "log", "--oneline", "-10")
	out, _ := cmd.CombinedOutput()
	t.Logf("Recent commits:\n%s", string(out))

	writeLiveTestReport(t, runID, workspaceDir, storeDir, s, events)
	assessQualityRaw(t, "feature-dev", "Featurly", "Add POST users/id/posts endpoint with validation, idempotency, and table-driven tests", runID, workspaceDir, storeDir, s, events, time.Since(start), reason, gitArtifactEvidence(t, workspaceDir))
}
