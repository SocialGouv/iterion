//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_AdrRechallenge runs the adr-rechallenge bot (ReArchi)
// against an existing ADR plus drifted code. ReArchi loads the ADR,
// surveys current code, frames keep/change/addendum arguments, and pauses
// at a human decision gate. The harness auto-resume driver answers that
// gate (it selects the first option, "keep"), so this exercises the
// load→survey→frame→decision path headlessly.
//
// Reliability invariants: load_adr/survey_code/frame_arguments fire and
// the human decision gate is reached + answered. The quality panel grades
// the framed arguments + value.
//
// Requires: claude CLI. Expected: ~10-25 min.
func TestLive_Bot_AdrRechallenge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")

	workspaceDir, err := os.MkdirTemp("", "iterion-adr-rechallenge-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	adrPath := "docs/adr/001-in-memory-storage.md"
	seedGitRepo(t, workspaceDir)
	writeWorkspaceFiles(t, workspaceDir, map[string]string{
		"go.mod": "module iterion-live-fixture\n\ngo 1.25\n",
		adrPath: `# 1. In-memory storage

Date: 2025-01-01
Status: accepted

## Context
The service is small and dependency-free.

## Decision
Use an in-memory map for storage. No database.

## Consequences
Data is lost on restart; acceptable for the prototype.
`,
		"store.go": `package fixture

// NOTE: code has since drifted toward needing persistence (see TODO).
// TODO: data loss on restart is now a problem; consider SQLite.
type Store struct{ m map[string]string }
`,
	})
	gitCommitAll(t, workspaceDir, "chore: seed ADR + drifted code")

	vars := map[string]interface{}{
		"workspace_dir": workspaceDir,
		"adr_path":      adrPath,
		"scope_notes":   "Code drifted toward needing persistence; reconsider the in-memory decision.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-adr-rechallenge",
		botFile:      "adr-rechallenge/main.bot",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      25 * time.Minute,
		autoResume:   true, // human_decision (+ human_commit_gate on addendum path)
		maxResumes:   6,
	})

	assertNodesFinished(t, res.events, "load_adr", "survey_code", "frame_arguments", "human_decision")
	dec, _ := lastNodeOutput(res.events, "human_decision")
	frame, _ := lastNodeOutput(res.events, "frame_arguments")
	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "adr-rechallenge",
		persona:       "ReArchi",
		primaryFamily: "anthropic",
		task:          "Re-challenge an in-memory-storage ADR against drifted code (now needing persistence); frame keep/change/addendum arguments.",
		workProduct:   "## decision\n" + sprintAny(dec) + "\n\n## framed arguments\n" + sprintAny(frame),
	})
}
