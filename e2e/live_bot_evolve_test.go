//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_Evolve runs the evolve bot (Evoly) against a small repo:
// it surveys maturity, synthesizes an architectural vision (auto-approved
// via the harness auto-resume driver), and emits a strategic backlog of
// board issues. Read-only on code; store + memory + board are isolated to
// the temp workspace.
//
// Reliability invariants: survey + emit_backlog fire, emit_backlog
// produces a non-empty created_issues with no hallucinated bot assignees.
// Then the quality panel grades the vision/backlog + value.
//
// Requires: claude CLI (review_claude) + OpenAI (survey/synthesize/emit).
// Expected: ~20-40 min. (Evoly has previously hit gpt context limits — a
// failure here is a real reliability signal, not a test bug.)
func TestLive_Bot_Evolve(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpenAI(t)
	t.Setenv("ITERION_TEST_STORE_DIR", "workspace") // isolate board + memory

	workspaceDir, err := os.MkdirTemp("", "iterion-evolve-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	seedGoModuleFixture(t, workspaceDir, map[string]string{
		"calc.go":   "package calc\n\nfunc Add(a, b int) int { return a + b }\nfunc Div(a, b int) int { return a / b }\n",
		"README.md": "# calc\n\nA tiny calculator library. No tests, no CI, no error handling.\n",
	})

	vars := map[string]any{
		"workspace_dir": workspaceDir,
		"scope_notes":   "Tiny untested lib; propose a pragmatic evolution toward reliability + tooling.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-evolve",
		bundleDir:    "../bots/evolve",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      40 * time.Minute,
		autoResume:   true, // investigate (ask_user) + human_review_vision + ask_continue
		maxResumes:   14,
	})

	assertNodesFinished(t, res.events, "survey", "emit_backlog")
	assertOutputFieldsNonEmpty(t, res.events, "emit_backlog", "created_issues")
	assertNoHallucinatedAssignees(t, res.events, "emit_backlog")

	emit, _ := lastNodeOutput(res.events, "emit_backlog")
	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "evolve",
		persona:       "Evoly",
		primaryFamily: "openai",
		task:          "Survey a tiny untested lib, synthesize a vision, and emit a strategic backlog of board issues.",
		workProduct:   "## summary\n" + sprintAny(emit["summary"]) + "\n\n## created_issues\n" + sprintAny(emit["created_issues"]),
	})
}
