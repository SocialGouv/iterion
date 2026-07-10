//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_DepUpdateGuard runs the dep-update-guard bot (Vetty)
// against a Dependabot-style PR branch that bumps a dependency. Vetty
// security-audits the bumped version, aligns code if needed, validates the
// build, and commits onto the PR branch (no merge). It runs in-place on
// the checked-out branch inside sandbox-sec (no worktree).
//
// Reliability invariants: prepare/security_audit/align fire and the audit
// produces a verdict. Board mirroring + forge posting are disabled
// (post_to_board=false, pr_url empty). The quality panel grades the audit
// + alignment + value.
//
// Requires: claude CLI + docker w/ iterion-sandbox-sec:edge. Expected:
// ~20-45 min.
func TestLive_Bot_DepUpdateGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireDockerImage(t, "ghcr.io/socialgouv/iterion-sandbox-sec:edge")

	workspaceDir, err := os.MkdirTemp("", "iterion-dep-update-guard-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	// base = old pin; PR branch = the bump Vetty must vet.
	base := map[string]string{
		"package.json": "{\n  \"name\": \"fixture\",\n  \"dependencies\": {\n    \"lodash\": \"4.17.4\"\n  }\n}\n",
	}
	branch := map[string]string{
		"package.json": "{\n  \"name\": \"fixture\",\n  \"dependencies\": {\n    \"lodash\": \"4.17.21\"\n  }\n}\n",
	}
	seedBranchDiffFixture(t, workspaceDir, "main", base, branch, "dependabot/npm/lodash-4.17.21")

	vars := map[string]any{
		"workspace_dir": workspaceDir,
		"base_ref":      "main",
		"pr_url":        "", // no forge → feedback post skipped
		"post_to_board": false,
		"scope_notes":   "Dependabot bump lodash 4.17.4 → 4.17.21.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-dep-update-guard",
		botFile:      "dep-update-guard/main.bot",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      45 * time.Minute,
		withWorkDir:  true, // sandbox bind-mount of the PR branch
	})

	assertNodesFinished(t, res.events, "prepare", "security_audit")
	audit, _ := lastNodeOutput(res.events, "security_audit")
	if audit != nil {
		t.Logf("security_audit verdict=%v", audit["verdict"])
	}

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "dep-update-guard",
		persona:       "Vetty",
		primaryFamily: "anthropic",
		task:          "Security-audit a Dependabot lodash bump on the PR branch, align code if needed, and validate (no merge).",
		workProduct:   "## audit\n" + sprintAny(audit) + "\n\n" + worktreeArtifactEvidence(t, workspaceDir),
	})
}
