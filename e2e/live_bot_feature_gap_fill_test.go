//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_FeatureGapFill runs the feature-gap-fill bot (Fini) against
// a partial implementation plus a gap_spec naming exactly what's missing.
// Fini surveys the existing state, plans, implements the gap, and converges
// via a cross-family review loop inside a worktree (worktree: auto +
// sandbox-full), committing the completion.
//
// Reliability invariants: survey_existing/plan/act fire and the review loop
// runs (a reviewer + streak_check); commits are logged. The quality panel
// grades the completion + value.
//
// Requires: claude CLI + OpenAI + docker w/ iterion-sandbox-full:edge.
// Expected: ~40-70 min.
func TestLive_Bot_FeatureGapFill(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpenAI(t)
	requireDockerImage(t, "ghcr.io/socialgouv/iterion-sandbox-full:edge")

	workspaceDir, err := os.MkdirTemp("", "iterion-feature-gap-fill-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	// A partial implementation: Validate is a stub that always returns nil,
	// and there are no tests. The gap_spec names the missing behavior.
	seedGoModuleFixture(t, workspaceDir, map[string]string{
		"user.go": `package fixture

import "errors"

type User struct {
	Name  string
	Email string
}

// Validate should reject empty Name and Email containing no "@".
// TODO: not implemented — currently accepts everything.
func (u User) Validate() error {
	return nil
}

var _ = errors.New // keep errors imported for the implementation
`,
	})
	seedCommits := workspaceCommitCount(t, workspaceDir)

	gapSpec := "Implemented: User struct + a Validate() stub that returns nil. " +
		"Missing: Validate must return a non-nil error when Name is empty, and when " +
		"Email contains no '@'. Also missing: table-driven tests in user_test.go covering " +
		"valid input, empty name, and email without '@'."
	vars := map[string]interface{}{
		"gap_spec":    gapSpec,
		"scope_notes": "Complete User.Validate per the gap_spec and add tests.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feature-gap-fill",
		botFile:      "feature-gap-fill/main.bot",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      70 * time.Minute,
		withWorkDir:  true,
	})

	assertNodesFinished(t, res.events, "survey_existing", "plan", "act")
	if countFinished(res.events, "reviewer_claude")+countFinished(res.events, "reviewer_gpt") == 0 {
		t.Errorf("expected at least one reviewer to fire")
	}
	t.Logf("commits after run: %d (seed %d)", workspaceCommitCount(t, workspaceDir), seedCommits)

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "feature-gap-fill",
		persona:       "Fini",
		primaryFamily: "anthropic",
		task:          "Complete a stubbed User.Validate (reject empty name / bad email) + add tests, per a gap_spec.",
		workProduct:   worktreeArtifactEvidence(t, workspaceDir),
	})
}
