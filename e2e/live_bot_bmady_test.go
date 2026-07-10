//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_Bmady runs the bmady bot through its BMAD multi-persona
// pipeline (analyst → PM → architect → stories → dev → QA → final review)
// headlessly: the harness auto-resume driver answers each of the five
// human gates (approve PRD, approve arch, select stories, ship). Work
// happens in a worktree (worktree: auto).
//
// Reliability invariants: the analyst + PM personas fire and the pipeline
// flows through its gates to a terminal node (proving the multi-gate graph
// executes end-to-end). The quality panel grades whatever artifact
// resulted. NOTE: generic auto-resume selects no specific stories, so the
// dev step may do little — this test validates the pipeline executes, and
// the quality snapshot tracks how much real work it produced.
//
// Requires: claude CLI. Expected: ~25-50 min.
func TestLive_Bot_Bmady(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")

	workspaceDir, err := os.MkdirTemp("", "iterion-bmady-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	seedGoModuleFixture(t, workspaceDir, map[string]string{
		"doc.go": "// Package fixture is a tiny string utility library.\npackage fixture\n",
	})

	vars := map[string]any{
		"brief": "Add a Reverse(s string) string function to the fixture package, with table-driven tests.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-bmady",
		botFile:      "bmady/main.bot",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      50 * time.Minute,
		withWorkDir:  true,
		autoResume:   true,
		maxResumes:   12,
	})

	assertNodesFinished(t, res.events, "analyst", "pm")
	t.Logf("commits after run: %d", workspaceCommitCount(t, workspaceDir))

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "bmady",
		persona:       "Bmady",
		primaryFamily: "anthropic",
		task:          "Run the BMAD pipeline for a small feature (add Reverse + tests) through analyst/PM/arch/stories/dev/QA gates.",
		workProduct:   worktreeArtifactEvidence(t, workspaceDir),
	})
}
