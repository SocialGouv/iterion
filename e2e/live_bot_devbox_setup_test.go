//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_DevboxSetup runs the devbox-setup bot (Devy) against a
// polyglot repo with no devbox.json. Devy detects the stack, generates a
// devbox.json (+ devbox.lock via `devbox install`), and commits — inside a
// worktree (worktree: auto + sandbox-sec, which ships devbox/nix).
//
// Reliability invariants: detect_stack/generate_devbox/verify_devbox fire
// and generate_devbox emits non-empty devbox.json content. The quality
// panel grades the generated config + value.
//
// Requires: claude CLI + docker w/ iterion-sandbox-sec:edge. Expected:
// ~20-45 min.
func TestLive_Bot_DevboxSetup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireDockerImage(t, "ghcr.io/socialgouv/iterion-sandbox-sec:edge")

	workspaceDir, err := os.MkdirTemp("", "iterion-devbox-setup-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	// Polyglot repo (Go + Node), NO devbox.json → Devy must detect + author.
	seedGitRepo(t, workspaceDir)
	writeWorkspaceFiles(t, workspaceDir, map[string]string{
		"go.mod":       "module iterion-live-fixture\n\ngo 1.25\n",
		"main.go":      "package main\n\nfunc main() {}\n",
		"package.json": "{\n  \"name\": \"fixture\",\n  \"version\": \"1.0.0\",\n  \"packageManager\": \"pnpm@9.0.0\"\n}\n",
	})
	gitCommitAll(t, workspaceDir, "chore: seed polyglot fixture (no devbox)")
	seedCommits := workspaceCommitCount(t, workspaceDir)

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-devbox-setup",
		botFile:      "devbox-setup/main.bot",
		workspaceDir: workspaceDir,
		vars:         map[string]any{},
		inputs:       map[string]any{},
		timeout:      45 * time.Minute,
		withWorkDir:  true,
	})

	assertNodesFinished(t, res.events, "detect_stack", "generate_devbox")
	if gd, ok := lastNodeOutput(res.events, "generate_devbox"); ok {
		if dj, _ := gd["devbox_json"].(string); len(dj) < 10 {
			t.Errorf("expected non-empty devbox.json content from generate_devbox, got %q", dj)
		}
	}
	t.Logf("commits after run: %d (seed %d)", workspaceCommitCount(t, workspaceDir), seedCommits)

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "devbox-setup",
		persona:       "Devy",
		primaryFamily: "anthropic",
		task:          "Detect a Go+Node polyglot stack and author a devbox.json (+lock), committing it.",
		workProduct:   worktreeArtifactEvidence(t, workspaceDir),
	})
}
